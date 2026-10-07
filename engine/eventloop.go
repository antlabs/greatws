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

//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly

package engine

import (
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/antlabs/pulse/core"
	"golang.org/x/sys/unix"
)

// EventLoop 是一个 epoll/kqueue 事件循环。
//
// 一个循环被一个 goroutine 跑（Loop），负责把 fd 上的事件翻译成对 Handler
// 的调用。注册的 fd 用 ET（边缘触发）——这是 pulse 的默认，也是这套东西
// 快的原因：一次边缘把数据读干净，不用反复问内核"还有没有"。
type EventLoop struct {
	core.PollingApi

	parent *MultiEventLoop

	// maxEventNum 是一次 epoll_wait 最多拿多少事件。
	maxEventNum int

	log *slog.Logger
}

// Loop 跑事件循环，直到 Free。
func (el *EventLoop) Loop() {
	// WaitGroup 的 Add 在 Start 里做（Add 要先于 Wait），这里只负责 Done
	defer el.parent.loopsWg.Done()

	for {
		if el.parent.isFreed() {
			return
		}
		// 超时不能是 -1（永远等）：Free 是置标志位让循环自己退出，一直
		// 阻塞在 epoll_wait 里就看不到那个标志位。100ms 是"空闲时每秒醒
		// 十次看一眼"，代价可以忽略（事件来的时候立刻返回，不等超时）。
		_, err := el.Poll(100*time.Millisecond, func(fd int, state core.State, err error) {
			// io.EOF 不是"出错"，是"对端关了"。kqueue 那边尤其要紧：
			// 对端发 FIN 时它回调的是 cb(fd, WRITE, io.EOF)——状态是
			// WRITE 不是 READ，错误位带着 io.EOF。早先把 io.EOF 当成
			// 普通错误记一行日志就 return 了，OnClose 永远不会调
			// （实测：客户端发几个字节再 close，服务端一点反应都没有）。
			eof := errors.Is(err, io.EOF)
			if err != nil && !eof {
				if errors.Is(err, core.EAGAIN) {
					return
				}
				el.parent.err("apiPoll", "err", err.Error())
				return
			}

			c := el.parent.getConn(fd)
			if c == nil {
				// 连接已经关了（epoll 里可能还有一个待处理的事件）
				return
			}

			if eof {
				// 先把缓冲区里剩的数据交给协议，再关。协议那边可能还有
				// 半条报文要处理，直接关就丢了。
				if b := c.ReadBuffer(); len(b) > 0 && c.handler != nil {
					if n, derr := c.handler.OnData(c, b); derr == nil && n > 0 {
						c.ConsumeRead(n)
					}
				}
				c.closeWith(io.EOF)
				return
			}

			// 一个连接同时只有一个人在处理。处理期间又来的事件记在
			// pending 位上，处理完的那个取走再跑一轮——ET 的边缘只来
			// 一次，丢了就再也没有通知，连接会卡住。
			if !c.tryBusy() {
				if state.IsRead() {
					c.setPendingRead()
				}
				if state.IsWrite() {
					c.setPendingWrite()
				}
				return
			}

			el.processConn(c, state.IsRead(), state.IsWrite())
		})
		if err != nil {
			el.parent.err("apiPoll", "err", err.Error())
			return
		}
	}
}

// processConn 处理一个连接的一轮：读、喂给协议、写。
//
// 循环而不是递归：同一个连接连续有事件时（请求-响应就是这种），就地接着
// 跑省掉一次完整的投递和唤醒，也保证同一个连接还是串行的。
func (el *EventLoop) processConn(c *Conn, isRead, isWrite bool) {
	defer c.unbusy()

	for {
		if c.IsClosed() {
			return
		}
		if isWrite && c.NeedFlush() {
			if err := c.Flush(); err != nil {
				c.closeWith(err)
				return
			}
		}
		if isRead {
			if err := el.readAndDispatch(c); err != nil {
				c.closeWith(err)
				return
			}
		}

		// 处理期间又到的那些事件
		isRead = c.takePendingRead()
		isWrite = c.takePendingWrite()
		if !isRead && !isWrite {
			return
		}
	}
}

// readAndDispatch 读一次，把数据喂给协议的 OnData。
//
// 协议返回消化了多少，引擎调 ConsumeRead；返回 0 表示"还不够凑一条报文"，
// 数据留在缓冲区里，下次读到再喂——这不是错误。
func (el *EventLoop) readAndDispatch(c *Conn) error {
	_, readErr := c.Read()

	// 读到的东西全部喂给协议，直到协议不再消费。
	// 注意要先喂再处理 readErr：FIN 到的时候缓冲区里可能还有数据
	// （Read 里"先让协议消费完"那条契约），先把它们交出去。
	for {
		buf := c.ReadBuffer()
		if len(buf) == 0 {
			break
		}
		n, err := c.handler.OnData(c, buf)
		if err != nil {
			return err
		}
		if n <= 0 {
			// 协议说这段还不够，等下次读
			break
		}
		c.ConsumeRead(n)
		if n < len(buf) {
			// 协议没吃完这一整段，说明它自己知道后面还有（一条报文
			// 结束、后面那条不完整），交给下一次事件
			break
		}
	}

	// 读出错（含对端关了）：缓冲区已经空了，可以把错误交给上层了
	if readErr != nil {
		return readErr
	}
	return nil
}

func (el *EventLoop) del(c *Conn) {
	fd := c.Fd()
	el.parent.delConn(fd)
}

// addWrite 把 fd 的可写事件注册上去。
func (el *EventLoop) addWrite(c *Conn) error {
	return nil // ET 下 AddRead 已经带了 EPOLLOUT，见 multi_event_loops.go
}

// AddRead 给一条连接注册读事件。
func (el *EventLoop) AddRead(c *Conn) error {
	return el.PollingApi.AddRead(c.Fd())
}

// DelRead 取消读事件。
func (el *EventLoop) DelRead(c *Conn) error {
	return el.PollingApi.DelRead(c.Fd())
}

func closeFd(fd int) error {
	return unix.Close(fd)
}

// 让这些包被引用（别的构建组合和日志用得到）
