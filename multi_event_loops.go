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
package greatws

import (
	"context"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antlabs/pulse/core"
	_ "github.com/antlabs/task/task/stream"
	_ "github.com/antlabs/task/task/stream2"
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
	defNumLoops      = runtime.NumCPU()
)

// 这个函数会被调用两次
// 默认 1个event loop分发io事件， 多个parse loop解析websocket包
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

	if *m.parseInParseLoop {
		m.parseLoop = newTaskParse()
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
	m.safeConns.Add(fd, c)
	// m.loops[index].conns.Store(fd, c)
	if err := m.loops[index].AddRead(c.getFd()); err != nil {
		m.loops[index].del(c)
		return err
	}
	atomic.AddInt64(&m.curConn, 1)
	return nil
}
