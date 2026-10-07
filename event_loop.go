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
	// 每个分片一个批次: event loop 收上来的连接按 fd 取模直接放进对应
	// 分片的批里, 一轮结束一次性投出去。
	//
	// 这样投递时不用再分组(分组要一次 sync.Pool 的 Get/Put 加一遍复制),
	// 而且同一分片的连接在同一个批里, 谁先谁后由同一个解析 go 程顺序跑。
	//
	for !el.shutdown {
		// 这一轮有没有往解析分片投过任务。fnet 的做法(见它 loop.run
		// 的注释): 投过就让出 P, 因为 runtime 把被唤醒的 goroutine 排进
		// 唤醒它的那个 P 的 runqueue, 也就是这个事件循环自己的; 而它
		// 立刻又回去等下一轮事件, 从不 park, 那些 goroutine 只能等着被
		// 别的 P 偷走, 而别的 P 可能正闲着。没投过就不用让——让了也是
		// 空转。
		submitted := false
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

			// 开了解析池时收到就投, 不在这一轮里攒: 攒到轮末才投的话,
			// 轮尾那些连接要等这一轮 epoll 走完(可能几毫秒)才开始被处理,
			// 那个等待直接进 TP95/TP99。读写合成一个任务, 让它们还是在
			// 一个 go 程上跑。
			//
			// 同一个 fd 永远落到同一个分片, 那边按投递顺序取, 所以一个
			// 连接的事件顺序还是它发生的顺序。
			if pl := el.parent.parseLoop; pl != nil {
				pl.send(fd%len(pl.allTaskParse), parseTask{
					c:       c,
					isRead:  state.IsRead(),
					isWrite: state.IsWrite(),
				})
				submitted = true
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

		// 只在真投过任务时让 P: 无条件让的话, 空闲轮也在白白交 P,
		// 实测那一下把 TPS 从 2.5M 拉到 1.85M。
		if submitted && el.parent.parseLoop != nil && el.parent.gosched {
			runtime.Gosched()
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
