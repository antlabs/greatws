package bigws

import (
	"log/slog"
	"os"
	"runtime"
	"sync/atomic"
)

type MultiEventLoop struct {
	numLoops         int // 事件循环数量
	maxEventNum      int
	minBusinessGoNum int // 起多少个业务goroutine
	loops            []*EventLoop
	t                *task
	curConn          int64
	*slog.Logger
}

// 获取当前连接数
func (m *MultiEventLoop) GetCurConnNum() int64 {
	return atomic.LoadInt64(&m.curConn)
}

// 获取当前运行的任务数
func (m *MultiEventLoop) GetCurTaskNum() int64 {
	return m.t.getCurTask()
}

func (m *MultiEventLoop) initDefaultSettingBefore() {
	m.numLoops = 0
	m.maxEventNum = 10000
	m.minBusinessGoNum = 50
}

func (m *MultiEventLoop) initDefaultSettingAfter() {
	if m.numLoops == 0 {
		m.numLoops = runtime.NumCPU() / 4
		if m.numLoops == 0 {
			m.numLoops = 1
		}
	}

	if m.maxEventNum == 0 {
		m.maxEventNum = 256
	}

	if m.minBusinessGoNum == 0 {
		m.minBusinessGoNum = 50
	}
}

func NewMultiEventLoopMust(opts ...EvOption) *MultiEventLoop {
	m, err := NewMultiEventLoop(opts...)
	if err != nil {
		panic(err)
	}

	m.Logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))
	return m
}

// 创建一个多路事件循环
func NewMultiEventLoop(opts ...EvOption) (e *MultiEventLoop, err error) {
	m := &MultiEventLoop{}

	m.initDefaultSettingBefore()
	for _, o := range opts {
		o(m)
	}
	m.initDefaultSettingAfter()

	m.t = newTask(m.minBusinessGoNum)

	m.loops = make([]*EventLoop, m.numLoops)

	for i := 0; i < m.numLoops; i++ {
		m.loops[i], err = CreateEventLoop(m.maxEventNum)
		if err != nil {
			return nil, err
		}
		m.loops[i].parent = m
	}
	return m, nil
}

// 启动多路事件循环
func (m *MultiEventLoop) Start() {
	for _, loop := range m.loops {
		go loop.Loop()
	}
}

// 添加一个连接到多路事件循环
func (m *MultiEventLoop) add(c *Conn) error {
	index := c.getFd() % len(m.loops)
	m.loops[index].conns.Store(c.getFd(), c)
	if err := m.loops[index].addRead(c); err != nil {
		m.del(c)
		return err
	}
	atomic.AddInt64(&m.curConn, 1)
	return nil
}

// 添加一个可写事件到多路事件循环
func (m *MultiEventLoop) addWrite(c *Conn) error {
	index := c.getFd() % len(m.loops)
	if err := m.loops[index].addWrite(c.getFd()); err != nil {
		return err
	}
	m.loops[index].conns.LoadOrStore(c.getFd(), c)
	return nil
}

// 添加一个可写事件到多路事件循环
func (m *MultiEventLoop) delWrite(c *Conn) error {
	index := c.getFd() % len(m.loops)
	if err := m.loops[index].delWrite(c.getFd()); err != nil {
		return err
	}
	m.loops[index].conns.LoadOrStore(c.getFd(), c)
	return nil
}

// 从多路事件循环中删除一个连接
func (m *MultiEventLoop) del(c *Conn) {
	if c.fd == -1 {
		return
	}
	atomic.AddInt64(&m.curConn, -1)
	index := c.getFd() % len(m.loops)
	m.loops[index].conns.Delete(c.getFd())
	closeFd(c.getFd())
}

// 获取一个连接
func (m *MultiEventLoop) getConn(fd int) *Conn {
	index := fd % len(m.loops)
	v, ok := m.loops[index].conns.Load(fd)
	if !ok {
		return nil
	}
	return v.(*Conn)
}
