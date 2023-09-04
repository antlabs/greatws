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
	kqfd   int
	events []unix.Kevent_t
}

func (eventLoop *EventLoop) apiCreate() (err error) {
	var state apiState
	state.kqfd, err = unix.Kqueue()
	if err != nil {
		return err
	}
	eventLoop.apidata = &state
	return nil
}

func (eventLoop *EventLoop) apiResize(setSize int) {
	oldEvents := eventLoop.apidata.events
	newEvents := make([]unix.Kevent_t, setSize)
	copy(newEvents, oldEvents)
	eventLoop.apidata.events = newEvents
}

func (eventLoop *EventLoop) apiFree() {
	unix.Close(eventLoop.apidata.kqfd)
}

func (eventLoop *EventLoop) apiAddEvent(fd int, mask Action) (err error) {
	state := eventLoop.apidata
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

func (eventLoop *EventLoop) apiDelEvent(fd int, mask Action) (err error) {
	state := eventLoop.apidata
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

func (eventLoop *EventLoop) apiPoll(tv time.Duration) int {
	state := eventLoop.apidata

	retVal := 0
	numEvnets := 0

	if tv >= 0 {
		var timeout unix.Timespec
		timeout.Sec = int64(tv / time.Second)
		timeout.Nsec = int64(tv % time.Second)

		retVal, _ = unix.Kevent(state.kqfd, nil, state.events, &timeout)
	} else {
		retVal, _ = unix.Kevent(state.kqfd, nil, state.events, nil)
	}

	if retVal > 0 {
		numEvnets = retVal
		for j := 0; j < numEvnets; j++ {
			mask := 0
			e := &state.events[j]
			if e.Filter == unix.EVFILT_READ {
				mask |= READABLE
			}

			if e.Filter == unix.EVFILT_WRITE {
				mask |= WRITABLE
			}
			eventLoop.fired[j].fd = int(e.Ident)
			eventLoop.fired[j].mask = mask
		}
	}
	return numEvnets
}

func apiName() string {
	return "kqueue"
}
