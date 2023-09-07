package bigws

type EvOption func(e *MultiEventLoop)

// 开启几个事件循环
func WithEventLoops(num int) EvOption {
	return func(e *MultiEventLoop) {
		e.numLoops = num
	}
}

// 设置每个事件循环的最大事件数量
func WithMaxEventNum(num int) EvOption {
	return func(e *MultiEventLoop) {
		e.maxEventNum = num
	}
}
