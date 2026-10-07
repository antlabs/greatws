// Copyright 2023-2024 antlabs. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package websocket

import (
	"context"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antlabs/pulse/core"
	_ "github.com/antlabs/task/task"
)

type taskConfig struct {
	initCount int // 初始化的协程数
	min       int // 最小协程数
	max       int // 最大协程数
}

type multiEventLoopOption struct {
	numLoops int //起多少个event loop

	// 为何不设计全局池, 现在的做法是
	// fd是绑定到某个事件循环上的，
	// 任务池是绑定到某个事件循环上的，所以这里的任务池也绑定到对应的localTask上
	// 如果设计全局任务池，那么概念就会很乱，容易出错，也会临界区竞争
	configTask taskConfig
	// taskMode         taskMode
	level       slog.Level //控制日志等级
	maxEventNum int        //每次epoll/kqueue返回时，一次最多处理多少事件

	// parseInWorkerPool 让 event loop 只做事件的分发，websocket frame 的
	// 读取和解析放到 taskParse 的 goroutine 里面做，见 task_parse.go。
	// 默认开; WithParseInWorkerPool 是在它已经开了的时候的显式写法,
	// 也没有关掉它的开关——关掉是 WithParseInEventLoop。
	parseInWorkerPool bool
	// parseGoroutines 是解析 goroutine 的数量, 0 表示 NumCPU。
	// 只在 parseInWorkerPool 开着时有意义。
	parseGoroutines int
	// parsePinned 让解析 goroutine 绑核, 见 WithParsePinned。
	parsePinned bool
	// parseWorkersPerShard 是每个解析分片起几个常驻 worker, 0 表示 1。
	// 见 task_parse.go 的 workersPerShard。
	parseWorkersPerShard int
	// parseInEventLoop 关掉解析池, 让 event loop 自己读和解析。
	// 默认开解析池, 见 initDefaultSetting。
	parseInEventLoop bool
	// noGosched 关掉投递后的让出(P 上的 runtime.Gosched)。
	//
	// 投完任务后让出 P。默认关。
	//
	// fnet 是开的(见它 loop_unix.go 的注释): runtime 把被唤醒的 goroutine
	// 排到"唤醒它的那个 P"的 runq 上, 而事件循环从不 park, 那些 goroutine
	// 就得等别的 P 来偷; 让出 P 让它们立刻跑。
	//
	// 实测(无绑定 10000 连接 1KB echo, 交替 2 轮):
	//   关: TPS 2,098,100  Avg 4.67ms  TP95 10.88  TP99 14.91  CPU 1120.8%
	//   开: TPS 2,186,538  Avg 4.47ms  TP95 11.41  TP99 16.34  CPU 1041.4%
	// 开了之后吞吐 +4.2%、平均延迟 -4.3%、CPU -7.1%, 但 TP95/TP99 差
	// 5%/10%。是"多数更快、少数更慢"的分布变化, 不是净改善。
	//
	// 顺带否掉一个假设: 开不开 gosched 对调度器的 runq 堆积没影响
	// (schedtrace 里 "有 >2 堆积的行数" 都是 31~33)。所以无绑定环境那
	// 个长尾不是"分片 goroutine 拿不到 P"造成的。
	gosched bool
	// batchSize 是一批攒多少连接再投, 0 表示默认(parseBatchSize)。
	batchSize int
}

// 默认MultiEventLoop
var DefaultMultiEventLoop *MultiEventLoop

var defaultOnce sync.Once

func getDefaultMultiEventLoop() *MultiEventLoop {

	defaultOnce.Do(func() {
		DefaultMultiEventLoop = NewMultiEventLoopMust(WithEventLoops(0), WithMaxEventNum(256), WithLogLevel(slog.LevelError)) // epoll, kqueue
	})
	return DefaultMultiEventLoop
}

type MultiEventLoop struct {
	multiEventLoopOption //配置选项

	safeConns core.SafeConns[Conn]

	loops     []*EventLoop
	parseLoop *taskParse

	flag evFlag // 是否使用io_uring，目前没有使用

	stat // 统计信息
	*slog.Logger

	evLoopStart uint32

	ctx context.Context

	once sync.Once
}

var (
	defMaxEventNum   = 256
	defTaskMin       = 50
	defTaskMax       = 30000
	defTaskInitCount = 8
	defNumLoops      = defaultNumLoops()
)

// cpusPerEventLoop 是一个 event loop 默认等几个 CPU 的事件。
//
// event loop 只等事件、分发, 占不满一个核(profile: Poll 只占总 CPU 的
// 3.4%), 真正吃满核的是解析 goroutine。
//
// 试过调密(核数/2): 无 CPU 绑定下 6/12/24 个 loop 的 TPS 分别是
// 2,097,158 / 2,098,981 / 2,115,475, 差在 1% 内; 显式设成 12 与保持 6
// 的交替对照也无差异(2,123,040 vs 2,129,113)。所以维持 4 个核一个 loop。
const cpusPerEventLoop = 4

// defaultNumLoops 是没设置 WithEventLoops 时起的 event loop 数:
// NumCPU 除以 cpusPerEventLoop 向下取整, 至少一个。
func defaultNumLoops() int {
	return max(runtime.NumCPU()/cpusPerEventLoop, 1)
}

// parsePerCPU 是每个核配几个解析 goroutine。
//
// 5/3 而不是 1, 也不是按核数减 loop 数算: 解析 goroutine 有相当一部分
// 时间在等 socket(读)和等内核(写 syscall), 不是一直在算, 所以要超订
// 才能把核喂饱。超订多少是实测出来的——12 核上跑 1KB echo, 每核 5/3 个
// (12 核 → 20 个) 比每核一个高 24%, 五轮交错测量无一例外; 再多(每核
// 2.33 个, 28 个) 开始掉。
//
// 代价是尾延迟: 超订越多, 同一个核上排队的越多, TP99 越长。这里选的
// 是"吞吐接近最高、尾延迟还能接受"的点, 见 README 的性能一节。
const parsePerCPU = 5.0 / 3.0

// defaultParseGoroutines 是没设置 WithParseGoroutines 时的解析
// goroutine 数。至少一个。
func defaultParseGoroutines(loops int) int {
	return max(int(float64(runtime.NumCPU())*parsePerCPU+0.5), 1)
}

// 这个函数会被调用两次
// 默认开启 WithParseInWorkerPool 时, 多个event loop只分发io事件, 多个parse
// goroutine解析websocket包; 不开时 event loop 自己把读和解析都做了。
func (m *MultiEventLoop) initDefaultSetting() {

	if m.level == 0 {
		m.level = slog.LevelError //
	}
	if m.numLoops == 0 {
		m.numLoops = max(defNumLoops, 1)
	}

	if m.maxEventNum == 0 {
		m.maxEventNum = defMaxEventNum
	}

	if m.configTask.min == 0 {
		m.configTask.min = defTaskMin
	} else {
		m.configTask.min = max(m.configTask.min/(m.numLoops), 1)
	}

	if m.configTask.max == 0 {
		m.configTask.max = defTaskMax
	} else {
		m.configTask.max = max(m.configTask.max/(m.numLoops), 1)
	}

	if m.configTask.initCount == 0 {
		m.configTask.initCount = defTaskInitCount
	} else {
		m.configTask.initCount = max(m.configTask.initCount/(m.numLoops), 1)
	}

	if m.flag == 0 {
		m.flag = EVENT_EPOLL
	}

	// 默认让 event loop 只分发, 解析在 worker pool 里。之前是反过来,
	// 实测(12 核 1KB echo)默认走解析池后吞吐更高、尾延迟也更好。
	if !m.parseInEventLoop {
		m.parseInWorkerPool = true
	}
}

func NewMultiEventLoopMust(opts ...EvOption) *MultiEventLoop {
	m, err := NewMultiEventLoop(opts...)
	if err != nil {
		panic(err)
	}

	return m
}

// 创建一个多路事件循环
func NewMultiEventLoop(opts ...EvOption) (e *MultiEventLoop, err error) {
	m := &MultiEventLoop{}
	m.safeConns.Init(core.GetMaxFd())
	m.initDefaultSetting()
	for _, o := range opts {
		o(m)
	}
	m.initDefaultSetting()
	m.Logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: m.level}))

	if m.parseInWorkerPool {
		pg := m.parseGoroutines
		if pg <= 0 {
			pg = defaultParseGoroutines(m.numLoops)
		}
		m.parseLoop = newTaskParseWorkers(pg, m.parsePinned, m.parseWorkersPerShard)
	}

	m.ctx = context.Background()
	m.loops = make([]*EventLoop, m.numLoops)

	for i := 0; i < m.numLoops; i++ {
		m.loops[i], err = CreateEventLoop(m.maxEventNum, m.flag, m)
		if err != nil {
			return nil, err
		}
	}
	return m, nil
}

// 初始化一个多路事件循环,并且运行它
func NewMultiEventLoopAndStartMust(opts ...EvOption) (m *MultiEventLoop) {
	m = NewMultiEventLoopMust(opts...)
	m.Start()
	return m
}

// 启动多路事件循环
func (m *MultiEventLoop) Start() {

	m.once.Do(func() {
		for _, loop := range m.loops {
			go loop.Loop()
		}
		time.Sleep(time.Millisecond * 10)
		atomic.StoreUint32(&m.evLoopStart, 1)
	})
}

func (m *MultiEventLoop) Free() {
	for _, m := range m.loops {
		m.Free()
	}
}
func (m *MultiEventLoop) isStart() bool {
	return atomic.LoadUint32(&m.evLoopStart) == 1
}

func (m *MultiEventLoop) getEventLoop(fd int) *EventLoop {
	return m.loops[fd%len(m.loops)]
}

// 添加一个连接到多路事件循环
func (m *MultiEventLoop) add(c *Conn) error {
	fd := c.getFd()
	if fd == -1 {
		return nil
	}
	index := fd % len(m.loops)
	if m.parseLoop != nil {
		m.parseLoop.addFD(fd)
	}
	m.safeConns.Add(fd, c)
	// m.loops[index].conns.Store(fd, c)
	if err := m.loops[index].AddRead(c.getFd()); err != nil {
		m.loops[index].del(c)
		return err
	}
	atomic.AddInt64(&m.curConn, 1)
	return nil
}
