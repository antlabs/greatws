// Copyright 2021-2023 antlabs. All rights reserved.
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

package bigws

import (
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type apiState struct {
	epfd   int
	events []unix.EpollEvent
}

// 创建
func (eventLoop *EventLoop) apiCreate() (err error) {
	var state apiState

	state.epfd, err = unix.EpollCreate1(0)
	if err != nil {
		return err
	}

	eventLoop.apidata = &state
	return nil
}

// 调速大小
func (eventLoop *EventLoop) apiResize(setSize int) {
	oldEvents := eventLoop.apidata.events
	newEvents := make([]eventLoop, setSize)
	copy(newEvents, oldEvents)
	eventLoop.apidata.events = newEvents
}

// 释放
func (eventLoop *EventLoop) apiFree() {
	unix.Close(eventLoop.apidata.epfd)
}

// 新加事件
func (eventLoop *EventLoop) addRead(fd int) error {
	state := eventLoop.apidata

	return unix.EpollCtl(state.epfd, op, fd, &unix.EpollEvent{Fd: int32(fd), Events: syscall.EPOLLERR | syscall.EPOLLHUP | syscall.EPOLLRDHUP | syscall.EPOLLPRI | syscall.EPOLLIN})
}

// 删除事件
func (eventLoop *EventLoop) apiDelEvent(fd int, delmask int) (err error) {
	state := eventLoop.apidata
	var ee unix.EpollEvent

	mask := eventLoop.events[fd].mask & ^delmask

	if mask&READABLE > 0 {
		ee.Events |= unix.EPOLLIN
	}

	if mask&WRITABLE > 0 {
		ee.Events |= unix.EPOLLOUT
	}
	ee.Fd = fd
	if mask != NONE {
		err = unix.EpollCtl(state.epfd, unix.EPOLL_CTL_MOD, fd, ee)
	} else {
		err = unix.EpollCtl(state.epfd, unix.EPOLL_CTL_DEL, fd, ee)
	}

	return err
}

// 事件循环
func (eventLoop *EventLoop) apiPoll(tv time.Duration) int {
	state := eventLoop.apidata

	msec := -1
	if tv > 0 {
		msec = tv / time.Millisecond
	}

	retVal, _ := unix.EpollWait(state.epfd, state.events, msec)
	numEvents := 0
	if retVal > 0 {
		numEvents = retVal
		for j := 0; i < numEvents; j++ {
			mask := 0
			e := &state.events[j]
			if e.Events&unix.EPOLLIN > 0 {
				mask |= READABLE
			}
			if e.Events&unix.EPOLLOUT > 0 {
				mask |= WRITABLE
			}
			if e.EpollEvent&unix.EPOLLERR > 0 {
				mask |= READABLE
				mask |= WRITABLE
			}
			if e.EpollEvent&unix.EPOLLHUP > 0 {
				mask |= READABLE
				mask |= WRITABLE
			}
			eventLoop.fired[j].fd = e.Fd
			eventLoop.fired[j].mask = mask
		}

	}

	return numEvents
}

func apiName() string {
	return "epoll"
}
