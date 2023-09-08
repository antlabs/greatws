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

func (e *EventLoop) addRead(fd int) {
	e.mu.Lock()
	e.apidata.changes = append(e.apidata.changes, unix.Kevent_t{Ident: uint64(fd), Filter: unix.EVFILT_READ, Flags: unix.EV_ADD})
	e.mu.Unlock()
	e.trigger()
}

func (e *EventLoop) addWrite(fd int) {
	e.mu.Lock()
	e.apidata.changes = append(e.apidata.changes, unix.Kevent_t{Ident: uint64(fd), Filter: unix.EVFILT_WRITE, Flags: unix.EV_ADD})
	e.mu.Unlock()
	e.trigger()
}

func (e *EventLoop) apiAddEvent(fd int, mask Action) (err error) {
	state := e.apidata
	ke := make([]unix.Kevent_t, 0, 2)

	if mask&READABLE > 0 {
		ke = append(ke, unix.Kevent_t{Ident: uint64(fd), Filter: unix.EVFILT_READ, Flags: unix.EV_ADD})
	}

	if mask&WRITABLE > 0 {
		ke = append(ke, unix.Kevent_t{Ident: uint64(fd), Filter: unix.EVFILT_WRITE, Flags: unix.EV_ADD})
	}

	if len(ke) > 0 {
		_, err = unix.Kevent(state.kqfd, ke, nil, nil)
		if err != nil {
			return err
		}
	}
	return nil
}

func (e *EventLoop) apiDelEvent(fd int, mask Action) (err error) {
	state := e.apidata
	ke := make([]unix.Kevent_t, 0, 2)

	if mask&READABLE > 0 {
		ke = append(ke, unix.Kevent_t{Ident: uint64(fd), Filter: unix.EVFILT_READ, Flags: unix.EV_DELETE})
	}

	if mask&WRITABLE > 0 {
		ke = append(ke, unix.Kevent_t{Ident: uint64(fd), Filter: unix.EVFILT_WRITE, Flags: unix.EV_DELETE})
	}

	if len(ke) > 0 {
		_, err = unix.Kevent(state.kqfd, ke, nil, nil)
		if err != nil {
			return err
		}
	}

	return nil
}

func (e *EventLoop) apiPoll(tv time.Duration) int {
	state := e.apidata

	retVal := 0
	numEvents := 0

	var changes []unix.Kevent_t
	e.mu.Lock()
	changes = e.apidata.changes
	e.apidata.changes = nil
	e.mu.Unlock()
	if tv >= 0 {
		var timeout unix.Timespec
		timeout.Sec = int64(tv / time.Second)
		timeout.Nsec = int64(tv % time.Second)

		retVal, _ = unix.Kevent(state.kqfd, changes, state.events, &timeout)
	} else {
		retVal, _ = unix.Kevent(state.kqfd, changes, state.events, nil)
	}

	if retVal > 0 {
		numEvents = retVal
		for j := 0; j < numEvents; j++ {
			ev := &state.events[j]
			fd := int(ev.Ident)
			conn := e.parent.getConn(fd)
			if conn == nil {
				unix.Close(fd)
				continue
			}

			if ev.Filter == unix.EVFILT_READ {
				// 读取数据，这里要发行下websocket的解析变成流式解析
			}

			if ev.Filter == unix.EVFILT_WRITE {
				// 刷新下直接写入失败的数据
				conn.flushOrClose()
			}
		}
	}
	return numEvents
}

func apiName() string {
	return "kqueue"
}
