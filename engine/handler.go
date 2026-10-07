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

// Package engine 是 fio 的协议无关网络引擎：epoll/kqueue 事件循环、非阻塞
// 连接的读写、缓冲区、定时器。协议（websocket、http、http2、tls...）实现
// Handler，引擎把数据喂进来、把要写的东西交回去。
//
// 只有这个包碰系统调用和事件循环。协议包不直接调 epoll，也不自己管 socket——
// 那些和协议无关，写一遍就够；反过来，帧头、状态码、握手这些只该被它自己的
// 包看见。
//
// 事件循环用 github.com/antlabs/pulse：它把 epoll/kqueue 的差异、ET/LT、
// park 还是阻塞等待都封好了，websocket/ 也用它。这个包建在它上面，不重复
// 造那部分。
package engine

// Handler 是协议实现的接口。引擎按事件调它。
//
// 形状对齐 http-parser 那一系的解析器（httparser 也是这个形状）：
// "给你一段字节，告诉你我消化了多少"。这样协议不用自己管缓冲——半条报文
// 没凑齐就返回 0，引擎把它留在缓冲区里，下一次读到了接着喂。
type Handler interface {
	// OnOpen 连接就绪。accept 之后、任何数据到达之前调一次。
	OnOpen(c *Conn)

	// OnData 有数据可读。buf 是读缓冲区里还没被消费的那一段。
	//
	// 返回消化了多少字节。返回 0 表示"这段还不够凑出一条报文"，引擎会
	// 把它留着，下次读到了再喂——**不是错误**。
	//
	// 返回 error 会让引擎关掉连接。
	OnData(c *Conn, buf []byte) (int, error)

	// OnClose 连接关闭，只会调一次。err 是关闭原因。
	OnClose(c *Conn, err error)
}

// HandlerFunc 让 Handler 可以只用函数实现（测试、简单场景）。
type HandlerFunc struct {
	OpenFunc  func(c *Conn)
	DataFunc  func(c *Conn, buf []byte) (int, error)
	CloseFunc func(c *Conn, err error)
}

func (h *HandlerFunc) OnOpen(c *Conn) {
	if h.OpenFunc != nil {
		h.OpenFunc(c)
	}
}

func (h *HandlerFunc) OnData(c *Conn, buf []byte) (int, error) {
	if h.DataFunc != nil {
		return h.DataFunc(c, buf)
	}
	// 默认全部消费掉：不给回调就等于把数据丢掉
	return len(buf), nil
}

func (h *HandlerFunc) OnClose(c *Conn, err error) {
	if h.CloseFunc != nil {
		h.CloseFunc(c, err)
	}
}
