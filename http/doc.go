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

// Package http 是 fio 上的 HTTP/1.1 实现。
//
// 事件循环、连接 io、缓冲区、任务池复用 websocket/ 里的引擎，这个包只负责
// "这些字节是一个 HTTP 请求/响应"。
//
// 报文解析用 github.com/antlabs/httparser，不自己写：
//
//   - 它是回调式的（Execute(setting, buf) 边解析边回调），这个形状正好对得上
//     非阻塞 io——数据分几次到就喂几次，不用攒够一个完整报文再解
//   - 它有 ReadyUpgradeData()，专认从 HTTP 升级到别的协议（websocket 就是
//     这么升的），不用自己判 Upgrade 头
//   - 它做过安全边界（MaxHeaderSize），不用重新踩一遍头无限大的坑
//
// 规划中的内容：
//
//   - 请求解析：接 httparser 的 Setting 回调，攒出 Request
//   - 响应写出：复用引擎的部分写和积攒（写不出去的部分交给可写事件）
//   - keep-alive：一个连接上顺序处理多个请求
//   - 升级：ReadyUpgradeData() 为真时把连接交给 websocket 包
//
// 还没开始。
package http
