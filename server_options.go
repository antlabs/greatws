// Copyright 2023-2024 antlabs. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package greatws

type ServerOption func(*ConnOption)

type ConnOption struct {
	Config
}

// WithServerZeroCopyPayload 让 OnMessage 的 payload 直接指向读缓冲区,
// 不再每个消息拷一份出来。
//
// 只有满足下面全部条件时才走这条路, 别的情况照旧拷贝:
//   - 开了这个选项;
//   - 消息没有被压缩(rsv1), 也没有分片——分片的下一个分片到达时同一块
//     读缓冲区会被覆写;
//   - 回调是就地执行的(io 模式, c.task == nil), 也就是会在这个栈帧里
//     跑完; 投到线程池的回调活到别的时间点, 必须持有自己的内存。
//
// 也就是说回调里拿到的 []byte 只在回调返回之前有效。存下来、或者交给
// 别的 goroutine 之后再用, 会读到后续消息的数据。(这和 fnet 给
// OnData 的 data 是同一个契约, 它的注释也写着"只在本次回调内有效"。)
//
// 换来的是一个消息省掉一次 payload 长度的拷贝和一次内存池的取还。
func WithServerZeroCopyPayload() ServerOption {
	return func(o *ConnOption) {
		o.zeroCopyPayload = true
	}
}

// 2. 设置服务端支持的子协议
func WithServerSubprotocols(subprotocols []string) ServerOption {
	return func(o *ConnOption) {
		o.subProtocols = subprotocols
	}
}
