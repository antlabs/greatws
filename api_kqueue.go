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

//go:build darwin
// +build darwin

package bigws

import (
	"fmt"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type apiState struct {
	kqfd    int
	events  []unix.Kevent_t
	changes []unix.Kevent_t
}

func (e *EventLoop) apiCreate() (err error) {
	var state apiState
	state.kqfd, err = unix.Kqueue()
	if err != nil {
		return err
	}
	e.apidata = &state
	e.apidata.events = make([]unix.Kevent_t, 1024)

	_, err = unix.Kevent(state.kqfd, []unix.Kevent_t{{
		Ident:  0,
		Filter: unix.EVFILT_USER,
		Flags:  unix.EV_ADD | unix.EV_CLEAR,
	}}, nil, nil)
	return nil
}

func (e *EventLoop) apiResize(setSize int) {
	oldEvents := e.apidata.events
	newEvents := make([]unix.Kevent_t, setSize)
	copy(newEvents, oldEvents)
	e.apidata.events = newEvents
}

func (e *EventLoop) apiFree() {
	unix.Close(e.apidata.kqfd)
}

// 在另外一个线程唤醒kqueue
func (e *EventLoop) trigger() {
	unix.Kevent(e.apidata.kqfd, []unix.Kevent_t{{Ident: 0, Filter: unix.EVFILT_USER, Fflags: unix.NOTE_TRIGGER}}, nil, nil)
}

// 新加读事件
func (e *EventLoop) addRead(fd int) {
	e.mu.Lock()
	e.apidata.changes = append(e.apidata.changes, unix.Kevent_t{Ident: uint64(fd), Filter: unix.EVFILT_READ, Flags: unix.EV_ADD | unix.EV_CLEAR})
	e.mu.Unlock()
	e.trigger()
}

func (e *EventLoop) delWrite(fd int) {
	e.mu.Lock()
	e.apidata.changes = append(e.apidata.changes, unix.Kevent_t{Ident: uint64(fd), Filter: unix.EVFILT_WRITE, Flags: unix.EV_DELETE | unix.EV_CLEAR})
	e.mu.Unlock()
	e.trigger()
}

// 新加写事件
func (e *EventLoop) addWrite(fd int) {
	e.mu.Lock()
	e.apidata.changes = append(e.apidata.changes, unix.Kevent_t{Ident: uint64(fd), Filter: unix.EVFILT_WRITE, Flags: unix.EV_ADD | unix.EV_CLEAR})
	e.mu.Unlock()
	e.trigger()
}

func (e *EventLoop) del(fd int) {
	e.mu.Lock()
	e.apidata.changes = append(e.apidata.changes, unix.Kevent_t{Ident: uint64(fd), Flags: syscall.EV_DELETE, Filter: syscall.EVFILT_READ})
	e.mu.Unlock()
	e.trigger()
}

func (e *EventLoop) apiPoll(tv time.Duration) (retVal int, err error) {
	state := e.apidata

	var changes []unix.Kevent_t
	e.mu.Lock()
	changes = e.apidata.changes
	e.apidata.changes = nil
	e.mu.Unlock()
	if tv >= 0 {
		var timeout unix.Timespec
		timeout.Sec = int64(tv / time.Second)
		timeout.Nsec = int64(tv % time.Second)

		retVal, err = unix.Kevent(state.kqfd, changes, state.events, &timeout)
	} else {
		retVal, err = unix.Kevent(state.kqfd, changes, state.events, nil)
	}
	if err != nil {
		return 0, err
	}

	fmt.Printf("有新的事件发生 %d, err :%v\n", retVal, err)
	if retVal > 0 {
		for j := 0; j < retVal; j++ {
			ev := &state.events[j]
			fd := int(ev.Ident)
			fmt.Printf("fd :%d, filter :%x, flags :%x\n", fd, ev.Filter, ev.Flags)
			conn := e.parent.getConn(fd)
			if conn == nil {
				unix.Close(fd)
				continue
			}

			if ev.Filter == unix.EVFILT_READ {
				// 读取数据，这里要发行下websocket的解析变成流式解析
				_, _ = conn.processWebsocketFrame()
				if ev.Flags&unix.EV_EOF != 0 {
					fmt.Printf("conn.Close")
					conn.Close()
				}
			}

			if ev.Filter == unix.EVFILT_WRITE {
				// 刷新下直接写入失败的数据
				conn.flushOrClose()
			}

		}
	}
	return retVal, nil
}

func apiName() string {
	return "kqueue"
}
