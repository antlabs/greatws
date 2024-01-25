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

//go:build darwin || freebsd
// +build darwin freebsd

package greatws

import (
	"errors"
	"io"
	"time"

	"golang.org/x/sys/unix"
)

type apiState struct {
	kqfd    int
	events  []unix.Kevent_t
	changes []unix.Kevent_t
}

func (e *EventLoop) apiCreate(flag evFlag) (err error) {
	var state apiState
	state.kqfd, err = unix.Kqueue()
	if err != nil {
		return err
	}
	e.apiState = &state
	e.apiState.events = make([]unix.Kevent_t, 1024)

	return err
}

func (e *EventLoop) apiFree() {
	if e.apiState != nil {
		unix.Close(e.apiState.kqfd)
	}
}

// 新加读事件
func (e *EventLoop) addRead(c *Conn) error {
	fd := c.getFd()
	if fd == -1 {
		return nil
	}

	_, err := unix.Kevent(e.kqfd, []unix.Kevent_t{
		{Ident: uint64(fd), Flags: unix.EV_ADD, Filter: unix.EVFILT_READ},
		{Ident: uint64(fd), Flags: unix.EV_ADD, Filter: unix.EVFILT_WRITE},
	}, nil, nil)
	return err
}

func (e *EventLoop) delWrite(c *Conn) (err error) {
	return nil
}

// 新加写事件
func (e *EventLoop) addWrite(c *Conn) error {
	return nil
}

func (e *EventLoop) apiPoll(tv time.Duration) (retVal int, err error) {
	state := e.apiState

	var timeout *unix.Timespec
	if tv >= 0 {
		var tempTimeout unix.Timespec
		tempTimeout.Sec = int64(tv / time.Second)
		tempTimeout.Nsec = int64(tv % time.Second)
		timeout = &tempTimeout
	}

	retVal, err = unix.Kevent(state.kqfd, nil, state.events, timeout)
	if err != nil {
		if errors.Is(err, unix.EINTR) {
			return 0, nil
		}
		return 0, err
	}

	if retVal > 0 {
		for j := 0; j < retVal; j++ {
			ev := &state.events[j]
			fd := int(ev.Ident)

			conn := e.getConn(fd)
			if conn == nil {

				unix.Close(fd)
				e.parent.Logger.Debug("conn is nil", "fd", fd)
				continue
			}

			if ev.Filter == unix.EVFILT_READ {
				if ev.Flags&unix.EV_EOF != 0 {
					conn.closeWithLock(io.EOF)
					continue
				}
				// 读取数据，这里要发行下websocket的解析变成流式解析
				err = conn.processWebsocketFrame()
				if err != nil {
					conn.closeWithLock(err)
					continue
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

func (e *EventLoop) apiName() string {
	return "kqueue"
}
