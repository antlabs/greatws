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
		e.globalTask.initCount = initCount
		e.globalTask.min = min
		e.globalTask.max = max
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
