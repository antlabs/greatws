// Copyright 2023-2023 antlabs. All rights reserved.
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

import "time"

// linux 有两个api，一个是epoll，一个是io_uring
type apiState struct {
	epollState
	iouringState
}

// 创建
func (e *EventLoop) apiCreate() (err error) {
	var state apiState

	state.epollState.apiCreate()
	state.iouringState.apiCreate()
	e.apidata = &state
	return nil
}

// 释放
func (e *EventLoop) apiFree() {
	e.epollState.apiFree()
	e.iouringState.apiFree()
}

// 新增
func (e *EventLoop) addRead(fd int) error {
	e.epollState.addRead(fd)
	e.iouringState.addRead(fd)
}

func (e *EventLoop) addWrite(fd int) error {
	e.epollState.addWrite(fd)
	e.iouringState.addWrite(fd)
}

func (e *EventLoop) del(fd int) error {
	e.epollState.del(fd)
	e.iouringState.del(fd)
}

func (e *EventLoop) apiPoll(tv time.Duration) (retVal int, err error) {
	e.epollState.apiPoll(tv)
	e.iouringState.apiPoll(tv)
}

func (e.EventLoop) apiName() string {
	if e.epollState.isCreate {
		return "epoll"
	}
	return "io_uring"
}
