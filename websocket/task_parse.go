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
	"runtime"
	"sync"
	"sync/atomic"
)

// 一轮 epoll 收上来的一个就绪连接。
//
//	c      连接
//	isRead 这一轮它可读
//	isWrite 这一轮它可写
type parseTask struct {
	c       *Conn
	isRead  bool
	isWrite bool
	// ts 是投递时刻, 只有用 -tags fio_latprobe 编译时才有值
	// (见 lat_probe.go), 用来量"投递到开始处理"的等待。
	ts int64
}

// parseTask 的 isRead/isWrite 和 pendingEvents 用同一套位。
const (
	parseEventRead  int32 = 1
	parseEventWrite int32 = 2
)

// taskChanSize 是每个解析 goroutine 待处理任务的队列长度。
//
// 装单个任务, 所以这个值就是"能积压多少个连接"。取 8192: 够吸收几轮
// epoll 的突发(maxEventNum 是 1000), 又不至于让环本身太大。
const taskChanSize = 8192

type taskParse struct {
	allTaskParse []*taskParseNode
	// fds 是每个分片见过的不同 fd 数, tasks 是每个分片处理过的任务数,
	// blocked 是投递时队列已满的次数。诊断用。
	fds     []int64
	tasks   []int64
	blocked []int64
	// countTasks 决定 send 要不要记任务数, 见那里。默认不记。
	countTasks bool
	// pinned 决定解析 goroutine 要不要绑核, 见 WithParsePinned。
	pinned bool
	// workersPerShard 是每个分片起几个常驻 worker。默认 1(和以前一样)。
	//
	// 多个时靠 fnet 那套"逐跳唤醒": 一个 worker 处理任务前, 如果环里还有
	// 活就先唤醒下一个 worker 来接, 这样手上这个慢了也不挡住后面的。
	// 同一连接不被两个 worker 同时碰, 靠 Conn 的 busy 位(见 processOne)。
	workersPerShard int
}

func newTaskParse(n int) *taskParse {
	return newTaskParsePinned(n, false)
}

// newTaskParsePinned 起 n 个分片, pinned 决定解析 goroutine 要不要绑核。
func newTaskParsePinned(n int, pinned bool) *taskParse {
	return newTaskParseWorkers(n, pinned, 1)
}

// newTaskParseWorkers 起 n 个分片, 每个分片 workers 个常驻 worker。
func newTaskParseWorkers(n int, pinned bool, workers int) *taskParse {
	if n <= 0 {
		n = runtime.NumCPU()
	}
	// 强制 1: 多 worker 那条路要 Conn 的 busy 位做互斥(同一连接的两次
	// 事件会被两个 worker 取到, 而连接的无锁状态只能被一个碰), 而完整
	// 实现过之后实测比单 worker 慢——详见 event_loop.go 投递点的说明。
	//
	// 留着这个参数是为了让那条路的代码还在、以后想再试时不用重写;
	// 但传 >1 也不会真的起多个 worker, 免得又踩崩溃。
	if workers > 1 {
		workers = 1
	}
	if workers <= 0 {
		workers = 1
	}
	tp := &taskParse{
		pinned:          pinned,
		workersPerShard: workers,
		allTaskParse:    make([]*taskParseNode, n),
		fds:             make([]int64, n),
		tasks:           make([]int64, n),
		blocked:         make([]int64, n),
	}
	wg := sync.WaitGroup{}
	wg.Add(n * workers)

	tp.start(&wg)
	wg.Wait()
	return tp
}

func (t *taskParse) start(wg *sync.WaitGroup) {
	// 先把所有分片建好再启动: 每个分片的 run 会去别的分片上偷活(steal),
	// 边建边启动的话它会读到还没赋值的槽位。
	for i := 0; i < len(t.allTaskParse); i++ {
		t.allTaskParse[i] = &taskParseNode{
			ring:   newTaskRing(taskChanSize),
			notify: make(chan struct{}, t.workersPerShard),
			owner:  t,
		}
	}
	for i := 0; i < len(t.allTaskParse); i++ {
		for w := 0; w < t.workersPerShard; w++ {
			go t.allTaskParse[i].run(wg)
		}
	}
}

// WithParsePinned 让每个解析 goroutine 钉在自己的 OS 线程上, 不随调度
// 在核之间搬。
//
// 每次搬迁都要把 L1/L2 上这个连接的东西丢下——读缓冲、解出来的帧、
// socket 在内核里的那份 cache——搬到新核上重新冷启动。实测(10k 连接、
// 1KB echo)开着的库每秒迁移两万多次, 而调度器本来就把它留在原核上的
// 库只有四百次。
//
// 代价是这些线程不能再被 runtime 复用去跑别的 goroutine, 所以
// parseGoroutines 要小于核数, 否则多出来的线程会和别的抢。
func WithParsePinned() EvOption {
	return func(e *MultiEventLoop) {
		e.parsePinned = true
	}
}

// send 把一个连接的任务投给分片 i。
//
// 投单个而不是一批: event loop 每收到一个就投一个, 那边的解析 goroutine
// 立刻就能开始读。攒成一批要等这一轮 epoll 走完才投, 而一轮可能有几千
// 个事件、走完要几毫秒, 轮尾那些连接白等那几毫秒, 直接进 TP95/TP99。
func (t *taskParse) send(i int, task parseTask) {
	if t.countTasks {
		atomic.AddInt64(&t.tasks[i], 1)
	}
	node := t.allTaskParse[i]
	if node.ring.push(task) {
		if node.waiting.Load() > 0 {
			node.wake()
		}
		return
	}
	// 环满了: 自旋等消费者腾出位置。
	//
	// 这里本来就不该经常走到——环按 taskChanSize 开, 那个值要能装下
	// 几轮的积压。走到这里说明消费者跟不上, 那 event loop 只能等,
	// 它上面的连接一起等。
	atomic.AddInt64(&node.blocked, 1)
	for !node.ring.push(task) {
		runtime.Gosched()
	}
	if node.waiting.Load() > 0 {
		node.wake()
	}
}

func (t *taskParse) addFD(fd int) {
	atomic.AddInt64(&t.fds[fd%len(t.allTaskParse)], 1)
}

// GetShardStats 返回每个分片的 (见过的新 fd 数, 处理的任务数, 投递阻塞次数)。
func (m *MultiEventLoop) GetShardStats() (fds, tasks, blocked []int64) {
	if m.parseLoop == nil {
		return nil, nil, nil
	}
	n := len(m.parseLoop.fds)
	fds = make([]int64, n)
	tasks = make([]int64, n)
	blocked = make([]int64, n)
	for i := range fds {
		fds[i] = atomic.LoadInt64(&m.parseLoop.fds[i])
		tasks[i] = atomic.LoadInt64(&m.parseLoop.tasks[i])
		blocked[i] = atomic.LoadInt64(&m.parseLoop.blocked[i])
	}
	return fds, tasks, blocked
}

type taskParseNode struct {
	ring *taskRing
	// blocked 是环满的次数, 诊断用。
	blocked int64
	// owner 指回自己所属的 taskParse, 拿诊断开关用。
	owner *taskParse
	// waiting 是"这个分片有几个人在睡"。多 worker 时必须是计数不能是
	// 布尔: 两个 worker 都睡着时, 投递方只 post 一个信号, 另一个就醒不来。
	// notify 的容量也是 worker 数, 保证每个睡着的都能收到一个。
	waiting atomic.Int32
	// notify 是唤醒用的信号量。传空结构体, 不搬数据——数据走 ring。
	// 比拿 channel 直接搬批次轻: 投递方 post 一个空值就走, 不用等
	// 接收方把数据接过去。
	notify chan struct{}
}

func (tpn *taskParseNode) run(wg *sync.WaitGroup) {
	wg.Done()
	// 绑核: 这个解析 goroutine 只在一个核上跑, 不随调度在核之间搬。
	// 每次搬迁都要丢掉 L1/L2 上这个连接的东西(读缓冲、解出来的帧、
	// socket 在内核里的那份 cache), 搬到新核上重新冷启动。
	//
	// 只在 WithParsePinned 开着时生效; 默认不绑——绑核会让这些 goroutine
	// 不能被 runtime 复用去跑别的活, 核数少于 goroutine 数时反而更差。
	if tpn.owner.pinned {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
	}
	for {
		// 先把环上排着的取干净
		for {
			pt, ok := tpn.ring.pop()
			if !ok {
				break
			}
			// fnet 的做法(见它 taskpool.go 的 work): 环里还有活就先叫一个
			// worker 来接, 再处理手上这个。手上这个可能要等 socket
			// (EAGAIN 重试)或者用户回调做了重活, 后面的任务不必等它。
			//
			// 这是"逐跳传播": 每个 worker 处理前都看一眼环, 有活就叫下一个,
			// 所以积压会沿着 worker 链一路传开, 不需要一个中心调度点。
			if tpn.ring.len() > 0 {
				tpn.notifyIfIdle()
			}
			tpn.processOne(&pt)
		}

		// 环空了。在去等之前, 先把 waiting 置上再看一眼——投递方在
		// 这之后 wake 我们, 那一次信号会落在下面的接收上, 不会丢。
		//
		// 置 waiting 到真正收到信号之间有一段窗口, 投递方在这期间进来
		// 的活会被 push 进 ring 并 wake; 那次 wake 与这次接收配成一对,
		// 所以不会丢也不会多。
		tpn.waiting.Add(1)
		// 记上之后再看一眼, 投递方可能刚好在这之前推完并 wake 过,
		// 那一次信号会落在下面的接收上, 不会丢。
		if tpn.ring.len() > 0 {
			tpn.waiting.Add(-1)
			continue
		}
		<-tpn.notify
		tpn.waiting.Add(-1)
	}
}

// wake 叫醒等着的解析 goroutine。已经在忙的话这次 post 会被下一次
// 等消费掉, 不会丢。
func (tpn *taskParseNode) wake() {
	select {
	case tpn.notify <- struct{}{}:
	default:
	}
}

// notifyIfIdle 在有 worker 睡着时唤醒一个(逐跳唤醒用)。
//
// 和 wake 的区别只是先看 waiting 计数——忙的时候不白投一次 channel。
func (tpn *taskParseNode) notifyIfIdle() {
	if tpn.waiting.Load() > 0 {
		tpn.wake()
	}
}

// processOne 处理一个连接的一轮: 读、解析、回调、写。
//
// 一轮就是"一次 epoll 说这个连接有事"。读和写放在一起, 让它们在一个
// go 程上跑, 连接的无锁状态(rbuf/rr/rw/curState)就只被这一个碰。
//
// 处理期间又到的那些事件被 event loop 记进了 pending 位(见
// Conn.notePending), 这里处理完取走它们自己再跑一轮——不补这一次的话
// ET 的边缘只来一次, 那些事件就丢了。
//
// 用循环而不是递归/再投一次: 同一个连接连续有事件时(请求-响应就是这种),
// 就地接着跑能省掉一次完整的投递 + 唤醒, 也保证同一连接还是串行的。
func (tpn *taskParseNode) processOne(pt *parseTask) {
	probeObserve(pt.ts)
	if pt.isRead {
		if err := pt.c.processWebsocketFrame(); err != nil {
			pt.c.Close()
			return
		}
	}
	if pt.isWrite && pt.c.needFlush() {
		pt.c.flush()
	}
}
