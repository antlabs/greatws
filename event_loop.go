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
package bigws

import (
	"context"
	"errors"
	"sync"
	"time"
)

type EventLoop struct {
	mu       sync.Mutex
	conns    sync.Map
	maxFd    int // highest file descriptor currently registered
	setSize  int // max number of file descriptors tracked
	apidata  *apiState
	shutdown bool
}

// 初始化函数
func CreateEventLoop(setSize int) *EventLoop {
	return &EventLoop{
		setSize: setSize,
		maxFd:   -1,
	}
}

// 柔性关闭所有的连接
func (e *EventLoop) Shutdown(ctx context.Context) error {
	return nil
}

func (el *EventLoop) GetSetSize() int {
	return el.setSize
}

func (el *EventLoop) Resize(setSize int) error {
	if setSize == el.setSize {
		return nil
	}

	if el.maxFd >= setSize {
		return errors.New("TODO 定义下错误消息")
	}

	el.apiResize(setSize)

	el.setSize = setSize

	for i := el.maxFd + 1; i < setSize; i++ {
	}
	return nil
}

func (el *EventLoop) StartLoop() {
	go el.Loop()
}

func (el *EventLoop) Loop() {
	for !el.shutdown {
		el.apiPoll(time.Duration(0))
	}
}

func (el *EventLoop) GetApiName() string {
	return apiName()
}
