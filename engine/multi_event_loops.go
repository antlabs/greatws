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

package engine

import (
	"log/slog"
	"os"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/antlabs/pulse/core"
)

// MultiEventLoop 是一组事件循环，连接按 fd 分片挂到其中一个上。
//
// 分片规则是 fd % len(loops)：同一个连接永远落在同一个循环上，所以它的
// 状态（读缓冲区、写缓冲、协议自己的状态机）只被那一个 goroutine 碰，
// 不用加锁。这是整套东西能快起来的根本。
type MultiEventLoop struct {
	loops []*EventLoop
	log   *slog.Logger

	// 连接表。分片加锁，见 pulse 的 SafeConns。
	conns core.SafeConns[Conn]
	// 连接对象池：accept 一个就取一个，关了就还回去。
	connPool sync.Pool

	numLoops    int
	maxEventNum int

	freed   int32
	started int32
	loopsWg sync.WaitGroup
	curConn int64
}

// Options
type options struct {
	numLoops    int
	maxEventNum int
	level       slog.Level
}

// Option 配 MultiEventLoop。
type Option func(*options)

// WithEventLoops 起几个事件循环。0 表示每个 CPU 一个。
//
// 不是越多越好：循环之间要抢 CPU，连接分片多了以后每个循环上的连接就少，
// 缓存局部性反而差。实测（12 核 / 10000 连接 / 1KB echo）循环数从 1 到 24
// 都试过，差别在噪声里。
func WithEventLoops(n int) Option {
	return func(o *options) { o.numLoops = n }
}

// WithMaxEventNum 一次 epoll_wait 最多拿多少事件。
func WithMaxEventNum(n int) Option {
	return func(o *options) { o.maxEventNum = n }
}

// WithLogLevel 日志级别。
func WithLogLevel(l slog.Level) Option {
	return func(o *options) { o.level = l }
}

const (
	defMaxEventNum = 256
)

// New 建一个多路事件循环。
func New(opts ...Option) (*MultiEventLoop, error) {
	o := options{}
	for _, f := range opts {
		f(&o)
	}
	if o.numLoops <= 0 {
		o.numLoops = runtime.NumCPU()
	}
	if o.maxEventNum <= 0 {
		o.maxEventNum = defMaxEventNum
	}

	m := &MultiEventLoop{
		numLoops:    o.numLoops,
		maxEventNum: o.maxEventNum,
	}
	m.log = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: o.level}))
	m.conns.Init(core.GetMaxFd())
	m.connPool.New = func() any { return &Conn{} }

	m.loops = make([]*EventLoop, o.numLoops)
	for i := range m.loops {
		el := &EventLoop{
			parent:      m,
			maxEventNum: o.maxEventNum,
			log:         m.log,
			// 注册连接（OnOpen 投递）走这条路，不是数据路径。
			// 压测里的 connect 风暴会瞬时挤进来一批，缓冲开大点，
			// 免得 accept 循环被卡住（见 runOnLoop）。
			tasks: make(chan func(), 4096),
		}
		api, err := core.Create(core.TriggerTypeEdge)
		if err != nil {
			return nil, err
		}
		el.PollingApi = api
		m.loops[i] = el
	}
	return m, nil
}

// NewAndStart 建一个并且跑起来。
func NewAndStart(opts ...Option) (*MultiEventLoop, error) {
	m, err := New(opts...)
	if err != nil {
		return nil, err
	}
	m.Start()
	return m, nil
}

// Start 把每个事件循环跑起来。
func (m *MultiEventLoop) Start() {
	if !atomic.CompareAndSwapInt32(&m.started, 0, 1) {
		return
	}
	// Add 必须在起 goroutine **之前**：WaitGroup 的规矩是"Add 要发生在
	// Wait 之前"，在 goroutine 里 Add 的话，Free 可能已经走到 Wait 了，
	// 那个 Add 就丢了（-race 报的就是这个）。
	for _, el := range m.loops {
		m.loopsWg.Add(1)
		go el.Loop()
	}
}

// Free 停掉所有事件循环。
//
// 关停是"置标志位、等循环自己退出"，不是从外面把 PollingApi 撕掉：
// pulse 的 Free 和 Poll 并发调是数据竞争（实测 -race 会报 api_kqueue.go
// 里 kqfd 的读写撞车），而且正在处理的连接会被从脚下抽走。
//
// 循环每轮开头看一次标志位，所以这里用一个短超时的 Poll 唤醒它们——
// 超时到了循环就回到开头看到 freed 并退出。等的是 WaitGroup，所以
// Free 返回时所有循环真的已经停了。
func (m *MultiEventLoop) Free() {
	if !atomic.CompareAndSwapInt32(&m.freed, 0, 1) {
		return
	}
	m.loopsWg.Wait()
	for _, el := range m.loops {
		el.PollingApi.Free()
	}
}

func (m *MultiEventLoop) isFreed() bool { return atomic.LoadInt32(&m.freed) == 1 }

// NumLoops 有几个事件循环。
func (m *MultiEventLoop) NumLoops() int { return len(m.loops) }

// NumConns 当前连接数。
func (m *MultiEventLoop) NumConns() int64 { return atomic.LoadInt64(&m.curConn) }

// Add 把一条非阻塞 fd 挂到引擎上，h 处理它的事件。
//
// accept 完拿到 fd，设成非阻塞，然后调这个。
//
// 引擎 Free 之后调它会返回 ErrClosed——accept 循环和 Free 是并发的
// （关监听 fd 之后 accept 可能还有一个已经拿到的连接要挂），不能假设
// 调用方先停 accept 再 Free。
func (m *MultiEventLoop) Add(fd int, h Handler) (*Conn, error) {
	if m.isFreed() {
		return nil, ErrClosed
	}
	c := m.connPool.Get().(*Conn)
	el := m.loops[fd%len(m.loops)]
	c.Init(fd, h, el)
	m.conns.Add(fd, c)
	if err := el.AddRead(c); err != nil {
		m.conns.Del(fd)
		m.connPool.Put(c)
		return nil, err
	}
	atomic.AddInt64(&m.curConn, 1)
	if h != nil {
		// OnOpen 投到事件循环的 goroutine 上跑。
		//
		// **不能在这里同步调**：Add 是 accept 循环（调用方的 goroutine）
		// 调的，而 OnOpen 里协议要初始化自己的状态（http2 建 Conn、
		// tls 建状态机、SetUserData……），那些状态之后只被事件循环碰
		// ——两边一写一读就是数据竞争。实测过：http2.ConnHandler 的
		// ch.conn 字段在 -race 下必报。
		//
		// 也不能往任务队列一扔就完事：投递和 epoll 事件之间**没有先后
		// 保证**——Add 返回时数据可能已经到了、事件已经排进 epoll。那
		// 就成了先跑 OnData、再跑 OnOpen，和上面那个竞争是一回事。
		//
		// 所以：任务里跑 OnOpen，跑完置 activated 位；事件处理那边看到
		// 位没置就把事件记成 pending，由 OnOpen 跑完时自己取走（见
		// activate 和 processConn）。
		el.runOnLoop(func() {
			el.activate(c)
		})
	}
	return c, nil
}

// activate 在事件循环的 goroutine 上跑 OnOpen，再把它跑完之前攒下的事件
// 补处理掉。
//
// 它跑在事件循环的 goroutine 上（runOnLoop 投过来的），所以和事件处理
// 是同一个 goroutine 串行的。
//
// **可重入**：Add 投的任务和事件处理两条路都可能调它（后者是"事件比
// 任务先到"的情况，见 EventLoop.processConn），谁先到谁跑 OnOpen，
// 后到的直接返回。两次调用都在同一个 goroutine 上，没有竞争。
func (el *EventLoop) activate(c *Conn) {
	if c.IsClosed() || c.isActivated() {
		return
	}

	// **busy 位归调用方管，这里不碰**。
	//
	// 两条路都会走到这儿，而它们对 busy 的所有权不一样：
	//
	//	1. Add 投的任务（runOnLoop -> activate）
	//	   这条路是自己进来的，没人持有 busy——但要占上，因为后面
	//	   processConn 也要占（见下面），而且"正在处理这个连接"的语义
	//	   本来就该成立。
	//
	//	2. processConn 里调（事件比任务先到）
	//	   这条路**调用方已经持有 busy 了**（Poll 回调里 tryBusy 拿的）。
	//	   这里要是再 tryBusy/unbusy 一次，会把人家持有的位清掉——之后
	//	   另一个 goroutine 就能同时进来处理同一条连接，状态直接乱掉
	//	   （实测：多线程下大量 TLS 握手卡在 Start，因为连接被两个
	//	   goroutine 交错处理）。
	//
	// 所以用"进来的时候有没有人持有"来判断：没有就自己占（并负责还），
	// 有就什么都别动。
	owned := c.tryBusy() // 返回 true 表示"之前没人持有，现在归我了"
	if c.handler != nil {
		c.handler.OnOpen(c)
	}
	if owned {
		c.unbusy()
	}

	// 置位并取回"OnOpen 之前就到的事件"。
	//
	// 有 pending 的话要接着处理（那些是 epoll 边缘，丢了就没有下一次
	// 通知了）。这时候自己要占 busy——但如果**调用方本来就持有**
	// （processConn 那条路），就不能再占：processConn 自己会接着跑，
	// 它拿着循环去取 pending。
	pendingRead, pendingWrite := c.setActivated()
	if (pendingRead || pendingWrite) && owned {
		if c.tryBusy() {
			el.processConn(c, pendingRead, pendingWrite)
		}
	}
}

func (m *MultiEventLoop) getConn(fd int) *Conn {
	return m.conns.Get(fd)
}

func (m *MultiEventLoop) delConn(fd int) {
	if fd < 0 {
		return
	}
	if c := m.conns.Get(fd); c != nil {
		m.conns.Del(fd)
		atomic.AddInt64(&m.curConn, -1)
		m.connPool.Put(c)
	}
}

func (m *MultiEventLoop) addWrite(c *Conn) {
	// 委托给连接所在的那个事件循环（见 EventLoop.addWrite 里
	// "为什么不能是空操作"的说明）
	if c.parent != nil {
		_ = c.parent.addWrite(c)
	}
}

func (m *MultiEventLoop) err(msg string, args ...any) {
	if m.log != nil {
		m.log.Error(msg, args...)
	}
}
