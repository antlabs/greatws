package bigws

type MultiEventLoop struct {
	numLoops int
	loops    []*EventLoop
}

func (m *MultiEventLoop) initDefaultSetting() {
	m.numLoops = 1
}

func CreateMultiEventLoop(opts ...Option) *MultiEventLoop {
	e := &MultiEventLoop{}

	for _, o := range opts {
		o(e)
	}

	e.loops = make([]*EventLoop, e.numLoops)
	for i := 0; i < e.numLoops; i++ {
		// e.loops[i]
		e.loops[i] = CreateEventLoop(1024 * 2)
	}
	return e
}
