// Copyright 2021-2024 antlabs. All rights reserved.
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
	"log/slog"
)

type EvOption func(e *MultiEventLoop)

// 开启几个事件循环, 控制io go程数量
func WithEventLoops(num int) EvOption {
	return func(e *MultiEventLoop) {
		e.numLoops = num
	}
}

// event loop 只做事件的分发，websocket frame 的读取和解析放到一组
// goroutine 里面做, 见 task_parse.go。默认不开, 这些工作都在 event loop
// 自己的 go 程上做。
//
// 解析的 goroutine 按 fd 取模分片, 一个连接固定落到一个上, 所以连接的
// 读缓冲和解析状态还是只被一个 go 程碰, 不需要加锁。
func WithParseInWorkerPool() EvOption {
	return func(e *MultiEventLoop) {
		e.parseInWorkerPool = true
	}
}

// 解析 goroutine 的数量, 默认 NumCPU。开了 WithParseInWorkerPool 才有意义。
//
// 解析 goroutine 和 event loop 抢同一批 P, 核数不够时每多一个就多一份
// 调度延迟, 所以有时比 NumCPU 少反而快。n <= 0 表示用默认值。
func WithParseGoroutines(n int) EvOption {
	return func(e *MultiEventLoop) {
		e.parseGoroutines = n
	}
}

// WithParseWorkersPerShard 让每个解析分片起 n 个常驻 worker, 默认 1。
//
// 多个 worker 之间用 fnet 那套"逐跳唤醒": 一个 worker 处理任务前看到环里
// 还有活, 就先叫醒下一个来接, 所以手上这个慢了也不挡住后面的。同一连接
// 不被两个 worker 同时碰, 靠 Conn 的 busy 位。n <= 0 用默认值 1。
func WithParseWorkersPerShard(n int) EvOption {
	return func(e *MultiEventLoop) {
		e.parseWorkersPerShard = n
	}
}

// 最小业务goroutine数量, 控制业务go程数量
// initCount: 初始化的协程数
// min: 最小协程数
// max: 最大协程数
func WithBusinessGoNum(initCount, min, max int) EvOption {
	return func(e *MultiEventLoop) {
		if initCount <= 0 {
			initCount = defTaskInitCount
		}

		if min <= 0 {
			min = defTaskMin
		}

		if max <= 0 {
			max = defTaskMax
		}
		e.configTask.initCount = initCount
		e.configTask.min = min
		e.configTask.max = max
	}
}

// 设置business go程池 对流量压测友好的模式
// func WithBusinessGoTrafficMode() EvOption {
// 	return func(e *MultiEventLoop) {
// 		e.taskMode = trafficMode
// 	}
// }

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
// func WithIoUring() EvOption {
// 	return func(e *MultiEventLoop) {
// 		e.flag |= EVENT_IOURING
// 	}
// }

// 关掉解析池, 让 event loop 自己读和解析 websocket frame。
//
// 默认是开的: event loop 只分发, 读取和解析在一组按 fd 分片的 goroutine
// 上做, 实测比 event loop 全包更快。这个选项给需要 event loop 独占
// 连接的场景用。
func WithParseInEventLoop() EvOption {
	return func(e *MultiEventLoop) {
		e.parseInEventLoop = true
	}
}

// 投完一批让出 P。默认不让, 见 multi_event_loops.go 里 gosched 的说明。
func WithGosched() EvOption {
	return func(e *MultiEventLoop) {
		e.gosched = true
	}
}

// 一次投给解析 goroutine 的连接数上限。默认 parseBatchSize。
// 调小(比如 1)就等于每条连接单个投递, 和 fib 的粒度一样。
func WithParseBatchSize(n int) EvOption {
	return func(e *MultiEventLoop) {
		e.batchSize = n
	}
}
