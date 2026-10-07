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

//go:build quicknet_latprobe

package websocket

// 延迟探针: 量"事件循环投递"到"解析 goroutine 开始处理"之间隔了多久。
//
// 用 -tags quicknet_latprobe 编译进来, 压测时读 /latprobe 看直方图。
// 打时间戳本身有开销(每消息一次 nanotime, 约 20ns), 所以只在探针构建里做。
//
// 为什么需要它: 无绑定环境下我们 TPS/Avg/CPU 都赢 fnet, 但 TP99 输 1.2ms。
// 队列、阻塞、分片倾斜、调度饥饿、内存管理五个假设都排除了, 那就得直接量
// "这一毫秒落在哪一段"——是投递前(epoll 回调里), 还是投递后等解析 goroutine。

import (
	"sync/atomic"
	"time"
)

var (
	// 按 100us 一档记投递到开始处理的等待时间
	probeBuckets [64]int64
	probeCount   int64
)

// probeMark 在投递前调用, 返回时间戳。
func probeMark() int64 {
	n := time.Now().UnixNano()
	atomic.AddInt64(&probeCount, 1)
	return n
}

// probeObserve 在处理开始时调用, 把等待时间记进直方图。
func probeObserve(start int64) {
	if start == 0 {
		return
	}
	d := time.Since(time.Unix(0, start)).Microseconds()
	b := d / 100 // 100us 一档
	if b < 0 {
		b = 0
	}
	if b >= int64(len(probeBuckets)) {
		b = int64(len(probeBuckets)) - 1
	}
	atomic.AddInt64(&probeBuckets[b], 1)
}

// GetLatProbe 返回直方图: 第 i 档表示 [i*100us, (i+1)*100us) 的样本数。
func (m *MultiEventLoop) GetLatProbe() ([]int64, int64) {
	out := make([]int64, len(probeBuckets))
	for i := range out {
		out[i] = atomic.LoadInt64(&probeBuckets[i])
	}
	return out, atomic.LoadInt64(&probeCount)
}
