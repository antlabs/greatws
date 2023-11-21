package greatws

import "log/slog"

type EvOption func(e *MultiEventLoop)

// 开启几个事件循环, 控制io go程数量
func WithEventLoops(num int) EvOption {
	return func(e *MultiEventLoop) {
		e.numLoops = num
	}
}

// 最小业务goroutine数量, 控制业务go程数量
// initCount: 初始化的协程数
// min: 最小协程数
// max: 最大协程数
func WithBusinessGoNum(initCount, min, max int) EvOption {
	return func(e *MultiEventLoop) {
		e.t.initCount = initCount
		e.t.min = min
		e.t.max = max
	}
}

// 设置日志级别
func WithLogLevel(level slog.Level) EvOption {
	return func(e *MultiEventLoop) {
		e.level = level
	}
}

// 设置每个事件循环一次返回的最大事件数量
func WithMaxEventNum(num int) EvOption {
	return func(e *MultiEventLoop) {
		e.maxEventNum = num
	}
}

// 暂时不可用
// 是否使用io_uring, 支持linux系统，需要内核版本6.2.0以上(以后只会在>=6.2.0的版本上测试)
func WithIoUring() EvOption {
	return func(e *MultiEventLoop) {
		e.flag |= EVENT_IOURING
	}
}
