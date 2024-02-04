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

//go:build linux
// +build linux

package greatws

import (
	"errors"
	"io"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// 垂直触发
	// 来自man 手册
	// When  used as an edge-triggered interface, for performance reasons,
	// it is possible to add the file descriptor inside the epoll interface (EPOLL_CTL_ADD) once by specifying (EPOLLIN|EPOLLOUT).
	// This allows you to avoid con‐
	// tinuously switching between EPOLLIN and EPOLLOUT calling epoll_ctl(2) with EPOLL_CTL_MOD.
	etRead      = uint32(unix.EPOLLERR | unix.EPOLLHUP | unix.EPOLLRDHUP | unix.EPOLLPRI | unix.EPOLLIN | unix.EPOLLOUT | unix.EPOLLET)
	etWrite     = uint32(0)
	etDelWrite  = uint32(0)
	etResetRead = uint32(0)

	// 一次性触发, TODO: 这里要看下是否需要，还是垂直触发+overflow fd记录
	etReadOneShot      = uint32(unix.EPOLLERR | unix.EPOLLHUP | unix.EPOLLRDHUP | unix.EPOLLPRI | unix.EPOLLIN | unix.EPOLLOUT | unix.EPOLLET | unix.EPOLLONESHOT)
	etWriteOneShot     = uint32(etReadOneShot)
	etDelWriteOneShot  = uint32(0)
	etResetReadOneShot = uint32(etReadOneShot)

	processWrite = uint32(unix.EPOLLOUT)
	processRead  = uint32(unix.EPOLLIN | unix.EPOLLRDHUP | unix.EPOLLHUP | unix.EPOLLERR)
)

type epollState struct {
	epfd   int
	events []unix.EpollEvent

	et      bool
	parent  *EventLoop
	rev     uint32
	wev     uint32
	dwEv    uint32 // delete write event
	resetEv uint32
}

func (e *epollState) getMultiEventLoop() *MultiEventLoop {
	return e.parent.parent
}

func getReadWriteDeleteReset(oneShot bool, et bool) (uint32, uint32, uint32, uint32) {
	if oneShot {
		return etReadOneShot, etWriteOneShot, etDelWriteOneShot, etResetReadOneShot
	}

	if et {
		return etRead, etWrite, etDelWrite, etResetRead
	}

	// if lt {
	// 	return ltRead, ltWrite, ltDelWrite, ltResetRead
	// }

	return 0, 0, 0, 0
}

// 创建epoll handler
func apiEpollCreate(parent *EventLoop) (la linuxApi, err error) {
	var e epollState
	e.epfd, err = unix.EpollCreate1(0)
	if err != nil {
		return nil, err
	}

	e.events = make([]unix.EpollEvent, 128)
	e.parent = parent
	e.rev, e.wev, e.dwEv, e.resetEv = getReadWriteDeleteReset(false, true)
	return &e, nil
}

// 释放
func (e *epollState) apiFree() {
	unix.Close(e.epfd)
}

// 新加读事件
func (e *epollState) addRead(c *Conn) error {
	if e.rev > 0 {
		fd := int(c.getFd())
		return unix.EpollCtl(e.epfd, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{
			Fd:     int32(fd),
			Events: e.rev,
		})
	}
	return nil
}

// 新加写事件
func (e *epollState) addWrite(c *Conn) error {
	if e.wev > 0 {

		fd := int(c.getFd())
		return unix.EpollCtl(e.epfd, unix.EPOLL_CTL_MOD, fd, &unix.EpollEvent{
			Fd:     int32(fd),
			Events: e.wev,
		})
	}

	return nil
}

// 删除写事件
func (e *epollState) delWrite(c *Conn) error {
	if e.dwEv > 0 {

		fd := int(c.getFd())
		return unix.EpollCtl(e.epfd, unix.EPOLL_CTL_MOD, fd, &unix.EpollEvent{
			Fd:     int32(fd),
			Events: e.dwEv,
		})
	}
	return nil
}

// 重装添加读事件
func (e *epollState) resetRead(c *Conn) error {
	fd := c.getFd()
	if e.resetEv > 0 {
		return unix.EpollCtl(e.epfd, unix.EPOLL_CTL_MOD, int(fd), &unix.EpollEvent{
			Fd:     int32(fd),
			Events: e.resetEv,
		})
	}
	return nil
}

// 删除事件
func (e *epollState) del(fd int) error {
	return unix.EpollCtl(e.epfd, unix.EPOLL_CTL_DEL, fd, &unix.EpollEvent{Fd: int32(fd)})
}

// 事件循环
func (e *epollState) apiPoll(tv time.Duration) (retVal int, err error) {
	msec := -1
	if tv > 0 {
		msec = int(tv) / int(time.Millisecond)
	}

	retVal, err = unix.EpollWait(e.epfd, e.events, msec)
	e.getMultiEventLoop().addPollEvNum()
	if err != nil {
		if errors.Is(err, unix.EINTR) {
			return 0, nil
		}
		return 0, err
	}
	numEvents := 0
	if retVal > 0 {
		numEvents = retVal
		for i := 0; i < numEvents; i++ {
			ev := &e.events[i]
			conn := e.parent.getConn(int(ev.Fd))
			if conn == nil {
				unix.Close(int(ev.Fd))
				continue
			}

			// 如果是关闭事件，直接关闭
			if ev.Events&unix.EPOLLRDHUP > 0 {
				conn.closeWithLock(io.EOF)
				continue
			}

			// if e.getMultiEventLoop().parseInParseLoop {
			// 	isRead := ev.Events&processRead > 0
			// 	isWrite := ev.Events&processWrite > 0
			// 	e.getMultiEventLoop().parseLoop.addTask(int(ev.Fd), func() bool {
			// 		if isRead {
			// 			err = conn.processWebsocketFrame()
			// 			if err != nil {
			// 				conn.closeWithLock(err)
			// 				return true
			// 			}
			// 		}

			// 		if isWrite {
			// 			// 刷新下直接写入失败的数据
			// 			conn.flushOrClose()
			// 		}

			// 		return true
			// 	})
			// 	continue
			// }
			if ev.Events&processRead > 0 {
				e.getMultiEventLoop().addReadEvNum()

				// 读取数据，这里要发行下websocket的解析变成流式解析
				err = conn.processWebsocketFrame()
				if err != nil {
					conn.closeWithLock(err)
				}
			}
			if ev.Events&processWrite > 0 {
				e.getMultiEventLoop().addWriteEvNum()
				// 刷新下直接写入失败的数据
				conn.flushOrClose()
			}
			if ev.Events&(unix.EPOLLERR|unix.EPOLLHUP|unix.EPOLLRDHUP) > 0 {
				conn.closeWithLock(io.EOF)
				continue
			}
		}

	}

	return numEvents, nil
}

func (e *epollState) apiName() string {
	return "epoll"
}
