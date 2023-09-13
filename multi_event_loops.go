package bigws

import "golang.org/x/sys/unix"

type MultiEventLoop struct {
	numLoops    int
	maxEventNum int
	loops       []*EventLoop
}

func (m *MultiEventLoop) initDefaultSetting() {
	m.numLoops = 1
	m.maxEventNum = 10000
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

	m.initDefaultSetting()
	for _, o := range opts {
		o(m)
	}

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
func (m *MultiEventLoop) add(c *Conn) {
	index := c.getFd() % m.numLoops
	m.loops[index].addRead(c.getFd())
	m.loops[index].conns.Store(c.getFd(), c)
}

// 从多路事件循环中删除一个连接
func (m *MultiEventLoop) del(c *Conn) {
	index := c.getFd() % m.numLoops
	m.loops[index].conns.Delete(c.getFd())
	unix.Close(c.getFd())
}

func (m *MultiEventLoop) getConn(fd int) *Conn {
	index := fd % m.numLoops
	v, ok := m.loops[index].conns.Load(fd)
	if !ok {
		return nil
	}
	return v.(*Conn)
}
