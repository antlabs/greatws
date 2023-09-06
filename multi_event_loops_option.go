package bigws

type Option func(e *MultiEventLoop)

// 开启几个事件循环
func WithEventLoops(num int) Option {
	return func(e *MultiEventLoop) {
		e.numLoops = num
	}
}
