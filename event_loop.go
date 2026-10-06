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
	"errors"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/antlabs/pulse/core"
	"github.com/antlabs/task/task/driver"
)

type evFlag int

const (
	EVENT_EPOLL evFlag = 1 << iota
	EVENT_IOURING
)

type EventLoop struct {
	maxFd   int // highest file descriptor currently registered
	setSize int // max number of file descriptors tracked
	core.PollingApi
	shutdown  bool
	parent    *MultiEventLoop
	localTask selectTasks
}

// 初始化函数
func CreateEventLoop(setSize int, flag evFlag, parent *MultiEventLoop) (e *EventLoop, err error) {
	e = &EventLoop{
		setSize: setSize,
		maxFd:   -1,
		parent:  parent,
	}

	var c driver.Conf
	c.Log = parent.Logger
	// 初始化任务池
	e.localTask = newSelectTask(parent.ctx, parent.configTask.initCount, parent.configTask.min, parent.configTask.max, &c)

	// TODO+
	// e.localTask.taskConfig = e.parent.configTask.taskConfig
	// e.localTask.taskMode = e.parent.configTask.taskMode
	// e.localTask.init()
	e.PollingApi, err = core.Create(core.TriggerType(flag))
	return e, err
}

// 柔性关闭所有的连接
func (e *EventLoop) Shutdown(ctx context.Context) error {
	return nil
}

func (el *EventLoop) Loop() {
	// 一轮 epoll 收上来的连接先进这个 batch, 一轮结束整批投给解析
	// goroutine, 而不是每个 fd 单独投一次。
	//
	// 批次的内存从池里取, 投出去就交出了所有权, 解析 goroutine 用完归还。
	batchSize := el.parent.batchSize
	if batchSize <= 0 {
		batchSize = parseBatchSize
	}
	batch := getBatch()
	for !el.shutdown {
		_, err := el.Poll(time.Duration(time.Second*100), func(fd int, state core.State, err error) {
			c := el.parent.safeConns.Get(fd)
			if err != nil {
				if errors.Is(err, core.EAGAIN) {
					return
				}
				if c != nil {
					c.Close()
				}
				el.parent.Error("apiPoll", "err", err.Error())
				return
			}

			if c == nil {
				el.parent.Logger.Error("apiPoll c is nil", "fd", fd)
				return
			}

			// 开了解析池时这里只攒事件, 读取和解析都在解析 goroutine 上
			// 做。读写合成一个任务, 让它们还是在一个 go 程上跑, 和没有
			// 解析池时一样。
			if el.parent.parseLoop != nil {
				batch = append(batch, parseTask{
					c:       c,
					isRead:  state.IsRead(),
					isWrite: state.IsWrite(),
				})
				// 攒够一批就先投, 免得一轮太长时后面的连接干等。
				if len(batch) >= batchSize {
					el.parent.parseLoop.addTask(batch)
					batch = getBatch()
				}
				return
			}

			if state.IsWrite() && c.needFlush() {
				c.flush()
			}

			if state.IsRead() {
				if err := c.processWebsocketFrame(); err != nil {
					c.Close()
				}
			}
		})
		if err != nil {
			el.parent.Error("apiPoll", "err", err.Error())
			return
		}

		if el.parent.parseLoop != nil && len(batch) > 0 {
			el.parent.parseLoop.addTask(batch)
			batch = getBatch()
			// 默认不让出 P, 见 multiEventLoopOption.gosched 的说明。
			if el.parent.gosched {
				runtime.Gosched()
			}
		}
	}
}

// 获取一个连接
func (m *EventLoop) getConn(fd int) *Conn {
	return m.parent.safeConns.Get(fd)
}

func (el *EventLoop) del(c *Conn) {
	fd := c.getFd()
	atomic.AddInt64(&el.parent.curConn, -1)
	el.parent.safeConns.Del(fd)
	// el.conns.Delete(fd)
	closeFd(fd)
}

func (el *EventLoop) delRead(c *Conn) error {
	return el.Del(c.getFd())
}

func (el *EventLoop) addWrite(c *Conn) error {
	return el.AddWrite(c.getFd())
}

func (el *EventLoop) GetApiName() string {
	return el.Name()
}
