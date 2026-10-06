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
}

// parseBatchSize 是一批最多攒多少连接。攒满就先投一批, 剩下的下一批。
const parseBatchSize = 128

// parseTask 的 isRead/isWrite 和 pendingEvents 用同一套位。
const (
	parseEventRead  int32 = 1
	parseEventWrite int32 = 2
)

// taskChanSize 是每个解析 goroutine 待处理批次的队列长度。
const taskChanSize = 256

// batchPool 装的是 []parseTask 的底层数组。
//
// 批次的 slice 一旦投给解析 goroutine, 所有权就归它; 它处理完还回来,
// 投递方才能再拿同一块内存装下一批。所以池有两个用处: 投递方拿空的
// 装, 解析方处理完归还。
var batchPool = sync.Pool{
	New: func() any {
		b := make([]parseTask, 0, parseBatchSize)
		return &b
	},
}

// getBatch 取一个空的批次缓冲。
func getBatch() []parseTask {
	p := batchPool.Get().(*[]parseTask)
	return (*p)[:0]
}

// putBatch 归还一个批次缓冲。调用方不能再碰它。
func putBatch(b []parseTask) {
	if cap(b) == 0 {
		return
	}
	b = b[:0]
	batchPool.Put(&b)
}

type taskParse struct {
	allTaskParse []*taskParseNode
	// fds 是每个分片见过的不同 fd 数, tasks 是每个分片处理过的任务数,
	// blocked 是投递时队列已满的次数。诊断用。
	fds     []int64
	tasks   []int64
	blocked []int64
	// countTasks 决定 send 要不要记任务数, 见那里。默认不记。
	countTasks bool
}

func newTaskParse(n int) *taskParse {
	if n <= 0 {
		n = runtime.NumCPU()
	}
	tp := &taskParse{
		allTaskParse: make([]*taskParseNode, n),
		fds:          make([]int64, n),
		tasks:        make([]int64, n),
		blocked:      make([]int64, n),
	}
	wg := sync.WaitGroup{}
	wg.Add(n)

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
			notify: make(chan struct{}, 1),
			owner:  t,
		}
	}
	for i := 0; i < len(t.allTaskParse); i++ {
		go t.allTaskParse[i].run(wg)
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

// addTask 投递一个批次的连接, 并交出其所有权。
//
// 批内的连接按 fd 取模分到不同的解析 goroutine 上: 同一个 fd 永远落到
// 同一个, 连接的读缓冲和解析状态还是只被一个 go 程碰, 不需要加锁。
//
// 这一点是这套调度成立的前提: 连接的无锁状态(rbuf/rr/rw/curState)全靠
// "一个 fd 只被一个解析 go 程碰"来保证。所以这里绝不能把连接投给别的
// 分片——曾试过让空闲分片去别的分片上偷活(work-stealing), 那会让两个
// go 程同时碰同一个连接, race detector 立刻报出来, 已回退。
//
// 投递方交出 batch 后不能再碰它, 本函数也不还给它——切片的内存要么被
// 分成几份交给各分片(各分片用完各自归还), 要么整批交给一个分片。
func (t *taskParse) addTask(batch []parseTask) {
	n := len(t.allTaskParse)
	if n == 1 {
		t.send(0, batch)
		return
	}

	// 按分片分组。每组的 slice 从池里取, 处理完由分片归还。
	// 分组表和组本身都用池里的, 免得每条消息都分配。
	g := groupsPool.Get().(*parseGroups)
	if cap(g.byShard) < n {
		g.byShard = make([][]parseTask, n)
	}
	g.byShard = g.byShard[:n]
	for i := range g.byShard {
		g.byShard[i] = nil
	}

	for i := range batch {
		idx := batch[i].c.getFd() % n
		if g.byShard[idx] == nil {
			g.byShard[idx] = getBatch()
		}
		g.byShard[idx] = append(g.byShard[idx], batch[i])
	}

	// batch 本身的内存还回去: 里面的元素已经复制到各组了。
	putBatch(batch)

	for i := 0; i < n; i++ {
		if len(g.byShard[i]) > 0 {
			t.send(i, g.byShard[i])
		}
	}
	groupsPool.Put(g)
}

var groupsPool = sync.Pool{
	New: func() any { return &parseGroups{} },
}

type parseGroups struct {
	byShard [][]parseTask
}

// send 把一个分片的批次投出去。队列满了就自旋, 不阻塞 event loop.
func (t *taskParse) send(i int, tasks []parseTask) {
	// 这里原来有一个 per-shard 的 atomic.AddInt64 记任务数; 它在每条
	// 消息至少一次的路径上, 而那个计数器是所有分片共享的 cache line,
	// 10 个分片轮流写它会来回弹。诊断不看这个数时就不记。
	if t.countTasks {
		atomic.AddInt64(&t.tasks[i], int64(len(tasks)))
	}
	node := t.allTaskParse[i]
	if node.ring.push(tasks) {
		if node.waiting.Load() {
			node.wake()
		}
		return
	}
	// 环满了: 自旋等消费者腾出位置, 不再阻塞 event loop.
	for !node.ring.push(tasks) {
		runtime.Gosched()
	}
	if node.waiting.Load() {
		node.wake()
	}
}

// addFD 记下一个分片见到的 fd
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
	// owner 指回自己所属的 taskParse, 拿诊断开关用。
	owner *taskParse
	// waiting 表示这个分片的 goroutine 已经没活可干, 投递方据此决定
	// 要不要 post sem。
	waiting atomic.Bool
	// notify 是唤醒用的信号量。传空结构体, 不搬数据——数据走 ring。
	// 比拿 channel 直接搬批次轻: 投递方 post 一个空值就走, 不用等
	// 接收方把数据接过去。
	notify chan struct{}
}

func (tpn *taskParseNode) run(wg *sync.WaitGroup) {
	wg.Done()
	for {
		// 先把环上排着的取干净
		for {
			batch, ok := tpn.ring.pop()
			if !ok {
				break
			}
			processBatch(batch)
			putBatch(batch)
		}

		// 环空了。在去等之前, 先把 waiting 置上再看一眼——投递方在
		// 这之后 wake 我们, 那一次信号会落在下面的接收上, 不会丢。
		//
		// 置 waiting 到真正收到信号之间有一段窗口, 投递方在这期间进来
		// 的活会被 push 进 ring 并 wake; 那次 wake 与这次接收配成一对,
		// 所以不会丢也不会多。
		tpn.waiting.Store(true)
		// 置位之后再看一眼, 投递方可能刚好在这之前推完并 wake 过,
		// 那一次信号会落在下面的接收上, 不会丢。
		if tpn.ring.len() > 0 {
			tpn.waiting.Store(false)
			continue
		}
		<-tpn.notify
		tpn.waiting.Store(false)
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

func processBatch(batch []parseTask) {
	for i := range batch {
		pt := &batch[i]
		if pt.isRead {
			if err := pt.c.processWebsocketFrame(); err != nil {
				pt.c.Close()
			}
		}
		if pt.isWrite && pt.c.needFlush() {
			pt.c.flush()
		}
		// 处理完了才放开这个连接, 让 event loop 能再投它。
		//
		// 处理期间到达的事件在 event loop 那边被挡下来、记进了
		// pendingEvents。这里先取走它们再放开标志: 取走之后、放开之前
		// 进来的事件会走 event loop 的正常投递, 放开之后进来的也是;
		// 只有"取走时读到的那些"要由这里补投一次, 不会漏也不会重。
		//
		// 漏一次就丢数据(ET 的边缘只来一次), 重了只是多读一次空的,
		// 所以顺序上先取后放。
	}
}

