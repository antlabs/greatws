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
package websocket

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
		// 连续空转 >= idleThreshold 轮才 park(让出 P); 否则用短超时走
		// 阻塞 epoll_wait, 免掉 park 那两次 netpoll。
		//
		// 门槛取 64: 高负载下事件循环几乎每轮都有事件, idleRounds 一直在
		// 0 附近, 一直走快路径; 真的闲下来(比如连接都处理完了)连续 64 轮
		// 没事件(每轮几十微秒, 合计几毫秒)才切 park, 那时让 P 才有意义。
		// 100ms: pulse 的 park 门槛是 1s, 这个值落在"短超时"档里,
		// 走阻塞 epoll_wait(有事件立刻返回, 没有就等到超时)。给 10ms 时
		// 测试里那种"两条消息间隔几十毫秒"的场景会超时, 100ms 留足余量。
		wait := time.Duration(time.Second * 100)
		gotEvent := 0
		_, err := el.Poll(wait, func(fd int, state core.State, err error) {
			gotEvent++
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
			//
			// 试过在多 worker 时改成随机分片(照 fnet 的 cheaprandn), 想让
			// "某个分片被抢调度时它名下 250 个连接一起慢"这个长尾散开:
			// 实测服务端几乎停摆(181,995 TPS / 57% CPU)。原因是随机分片
			// 后同一个连接的两个事件会落到不同分片, 一个抢到 busy 在处理,
			// 另一个抢不到就丢弃——而 ET 的边缘只来一次, 那个事件就此丢失,
			// 连接卡住。要支持随机分片得先有"抢不到就把它记下来、由正在
			// 处理的那个补做"的机制(fnet 的 scheduledBit + pending 位就是
			// 干这个的), 不能只加 busy 位。
			if pl := el.parent.parseLoop; pl != nil {
				pl.send(fd%len(pl.allTaskParse), parseTask{
					c:       c,
					isRead:  state.IsRead(),
					isWrite: state.IsWrite(),
					ts:      probeMark(),
				})
				submitted = true
				return
			}

			if state.IsWrite() && c.needFlush() {
				c.flush() // 内部拿锁：用户可能同时在 WriteMessage
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

// addWrite 让"这条连接还有积压要写"这件事能被内核再通知一次。
//
// **为什么不能直接用 AddWrite**：ET 模式下 pulse 的 AddWrite 是空操作
// （etAddWrite = 0）——它的设计是"AddRead 的时候把 EPOLLIN|EPOLLOUT
// 一起注册上"（man 手册里 ET 的那条用法），之后靠边缘通知。
//
// 但边缘的语义是"socket 从不可写变成可写时给一次"。有个场景它覆盖不到：
//
//	写 80KB -> 内核只吃了 40KB，剩 40KB 进 wbufList
//	-> 此时内核缓冲区刚腾空，socket 是"可写"状态，没有"变成可写"这个事件
//	-> 后续的写还是 EAGAIN（缓冲区又满了）
//	-> 等 EPOLLOUT —— 但边缘不会再来了，那条连接剩下的数据永远发不出去
//
// 实测：10 批 × 20 条 × 4KB 的批量写，10 次里挂 1 次，卡在随机位置
// （126/200、180/200 这种），尾部数据丢了。
//
// 修法：用 ResetRead（它的掩码 = AddRead，含 EPOLLOUT）。在 ET 下
// **EPOLL_CTL_MOD 会让 fd 重新进就绪队列**，等价于"再给我一次机会"。
func (el *EventLoop) addWrite(c *Conn) error {
	return el.AddWrite(c.getFd())
}

func (el *EventLoop) GetApiName() string {
	return el.Name()
}
