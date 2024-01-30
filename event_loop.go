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
package greatws

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

type evFlag int

const (
	EVENT_EPOLL evFlag = 1 << iota
	EVENT_IOURING
)

type EventLoop struct {
	conns     sync.Map // TODO 优化，后面换成b-tree
	maxFd     int      // highest file descriptor currently registered
	setSize   int      // max number of file descriptors tracked
	*apiState          // 每个平台对应的异步io接口/epoll/kqueue/iouring(暂时不加，除非io-uring性能超过epoll才加回来)
	shutdown  bool
	parent    *MultiEventLoop
	localTask task
}

// 初始化函数
func CreateEventLoop(setSize int, flag evFlag, parent *MultiEventLoop) (e *EventLoop, err error) {
	e = &EventLoop{
		setSize: setSize,
		maxFd:   -1,
		parent:  parent,
	}
	e.localTask.taskConfig = e.parent.configTask.taskConfig
	e.localTask.taskMode = e.parent.configTask.taskMode
	e.localTask.init()
	err = e.apiCreate(flag)
	return e, err
}

// 柔性关闭所有的连接
func (e *EventLoop) Shutdown(ctx context.Context) error {
	return nil
}

func (el *EventLoop) StartLoop() {
	go el.Loop()
}

func (el *EventLoop) Loop() {
	for !el.shutdown {
		_, err := el.apiPoll(time.Duration(time.Second * 100))
		if err != nil {
			el.parent.Error("apiPolll", "err", err.Error())
			return
		}
	}
}

// 获取一个连接
func (m *EventLoop) getConn(fd int) *Conn {

	v, ok := m.conns.Load(fd)
	if !ok {
		return nil
	}
	return v.(*Conn)
}

func (el *EventLoop) del(c *Conn) {
	fd := c.getFd()
	atomic.AddInt64(&el.parent.curConn, -1)
	el.conns.Delete(fd)
	closeFd(fd)
}
func (el *EventLoop) GetApiName() string {
	return el.apiName()
}
