// Copyright 2021-2024 antlabs. All rights reserved.
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
package unstream

import (
	"container/heap"
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antlabs/greatws/task/driver"
	"github.com/antlabs/greatws/window"
)

var _ driver.TaskDriver = (*task)(nil)
var _ driver.Tasker = (*task)(nil)
var _ driver.TaskExecutor = (*businessGo)(nil)

func init() {
	driver.Register("unstream", &task{})
}

var exitFunc = func() bool { return true }

type taskConfig struct {
	initCount int // 初始化的协程数
	min       int // 最小协程数
	max       int // 最大协程数
}

// task 模式
// 1. tps模式
// 2. 流量模式, TODO: 这个模式再压测下，如果数据不是足够好，可以去除
type taskMode int

const (
	tpsMode taskMode = iota
	trafficMode
)

type task struct {
	mu              sync.Mutex       // 锁
	public          chan func() bool // TODO: 公共任务chan，本来是想作为任务平衡的作用，目前没有启用，是否使用还要根据压测结果调整.
	window          window.Window    // 滑动窗口计数，用于判断是否需要新增go程
	taskConfig                       // task的最配置，比如初始化启多少协程，动态扩容时最小协程数和最大协程数
	curGo           int64            // 当前运行协程数
	curTask         int64            // 当前运行任务数
	allBusinessGo   allBusinessGo    // task的业务协程， 目前是最小堆
	stealID         uint32           // 窃取id
	taskMode        taskMode         // task的模式， 目前有三种
	businessChanNum int              // 各自go程收取任务数的chan的空量
	startOk         chan struct{}    // 至少有一个go程起来
	c               *driver.Conf
}

func (t *task) New(ctx context.Context, initCount, min, max int, c *driver.Conf) driver.Tasker {
	var t2 task
	t2.initCount = initCount
	t2.min = min
	t2.max = max
	t2.c = c
	t2.initInner()
	return &t2
}

// 获取当前go程数
func (t *task) GetGoroutines() int {
	return int(atomic.LoadInt64(&t.curGo))
}

func (t *task) nextStealID() uint32 {
	return (atomic.AddUint32(&t.stealID, 1) - 1) % uint32(len(t.allBusinessGo))
}

// 初始化
func (t *task) initInner() {
	if t.initCount == 0 {
		panic("initCount must be greater than 0")
	}

	t.public = make(chan func() bool, runtime.NumCPU())
	t.allBusinessGo = make([]*businessGo, 0, t.initCount)
	t.startOk = make(chan struct{}, 1)
	t.window.Init()
	go t.manageGo()
	go t.runConsumerLoop()
}

func (t *task) init() {
	t.businessChanNum = runtime.NumCPU() / 4
	if t.taskMode == trafficMode {
		t.businessChanNum = 1024
	}
	t.initInner()
	<-t.startOk // 等待至少有一个go程起来
}

// 消费者循环
func (t *task) consumer(steal *businessGo) {
	defer atomic.AddInt64(&t.curGo, -1)
	currBusinessGo := newBusinessGo(t.businessChanNum, t)
	t.mu.Lock()
	heap.Push(&t.allBusinessGo, currBusinessGo)
	t.mu.Unlock()

	// 窃取下任务
	if steal == nil {
		t.mu.Lock()
		steal = t.allBusinessGo[t.nextStealID()]
		t.mu.Unlock()
	}

	// 如果有任务，先窃取
	if len(steal.taskChan) > 0 {
		for {
			select {
			case f := <-steal.taskChan:
				if exit := t.runWork(currBusinessGo, f); exit {
					return
				}
			default:
				goto next
			}
		}
	next:
	}

	// 防止初始化太快，go程没有起来
	select {
	case t.startOk <- struct{}{}:
	default:
	}

	var f func() bool
	for {
		select {
		case f = <-t.public:
		case f = <-currBusinessGo.taskChan:
		}

		if exit := t.runWork(currBusinessGo, f); exit {
			return
		}
	}
}

func (t *task) runWork(currBusinessGo *businessGo, f func() bool) (exit bool) {
	atomic.AddInt64(&t.curTask, 1)
	if b := f(); b {
		if !currBusinessGo.canKill() {
			return false
		}
		t.mu.Lock()
		heap.Remove(&t.allBusinessGo, currBusinessGo.index)
		t.mu.Unlock()

		atomic.AddInt64(&t.curTask, -1)

		return true
	}
	atomic.AddInt64(&t.curTask, -1)
	return false
}

// 获取一个go程，如果是slice的话，最小连接数的方式
func (t *task) NewExecutor() driver.TaskExecutor {
	t.mu.Lock()
	defer t.mu.Unlock()
	v := t.allBusinessGo[0]
	v.addBinConnCount()
	heap.Fix(&t.allBusinessGo, v.index)
	return v
}

// 是否满了
func (t *task) isFull() bool {
	return atomic.LoadInt64(&t.curGo) >= int64(t.max)
}

func (t *task) addGoWithSteal(g *businessGo) bool {
	if atomic.LoadInt64(&t.curGo) >= int64(t.max) {
		return false
	}
	atomic.AddInt64(&t.curGo, 1)
	go func() {
		defer atomic.AddInt64(&t.curGo, -1)
		t.consumer(g)
	}()
	return true
}

// 新增go程
func (t *task) addGo() {
	t.addGoWithSteal(nil)
}

func (t *task) addGoNum(n int) {
	for i := 0; i < n; i++ {
		t.addGo()
	}
}

// 取消go程
func (t *task) cancelGoNum(sharkSize int) {
	if atomic.LoadInt64(&t.curGo) < int64(t.min) {
		return
	}
	for i := 0; i < sharkSize; i++ {
		if atomic.LoadInt64(&t.curGo) < int64(t.min) {
			return
		}
		t.public <- exitFunc
	}
}

// 需要扩容
func (t *task) needGrow() (bool, int) {
	if int(t.curGo) > t.max {
		return false, 0
	}

	curTask := atomic.LoadInt64(&t.curTask)
	curGo := atomic.LoadInt64(&t.curGo)
	avg := t.window.Avg()
	need := (float64(curTask)/float64(curGo)) > 0.8 && curGo > int64(avg)

	if need {

		if avg*2 < 8 {
			return true, 16
		}

		if avg*2 < 1024 {
			return true, int(avg * 2)
		}

		return true, int(float64(t.curGo) * 1.25)
	}

	return false, 0
}

func (t *task) needShrink() (bool, int) {
	// 小于最小值直接忽略收缩
	if int(t.curGo) <= t.min {
		return false, 0
	}

	curTask := atomic.LoadInt64(&t.curTask)
	curGo := atomic.LoadInt64(&t.curGo)

	need := (float64(curTask)/float64(curGo)) < 0.25 && curGo < int64(t.window.Avg())
	return need, int(float64(t.curGo) * 0.75)
}

// 管理go程
func (t *task) manageGo() {
	for {
		time.Sleep(time.Second * 1)
		// 当前运行的go程数
		curGo := atomic.LoadInt64(&t.curGo)
		// 记录当前运行的任务数
		t.window.Add(curGo)

		// 1分钟内不考虑收缩go程
		if need, shrinkSize := t.needShrink(); need {
			t.cancelGoNum(shrinkSize)
		} else if need, newSize := t.needGrow(); need {
			t.addGoNum(newSize - int(curGo))
		}
	}
}

func (t *task) getGoBusiness(addr uintptr) *businessGo {
	t.mu.Lock()
	node := t.allBusinessGo[int(addr)%len(t.allBusinessGo)]
	t.mu.Unlock()
	return node
}

// 运行任务
func (t *task) runConsumerLoop() {
	atomic.AddInt64(&t.curGo, int64(t.initCount))
	for i := 0; i < t.initCount; i++ {
		go t.consumer(nil)
	}
}

// func (t *task) rebindGoFast(c *Conn) {
// 	t.mu.Lock()
// 	minTask := t.allBusinessGo[0]
// 	src := c.getCurrBindGo()
// 	if src.bindConnCount > minTask.bindConnCount {
// 		src.subBinConnCount()
// 		minTask.addBinConnCount()
// 		c.setCurrBindGo(minTask)
// 		heap.Fix(&t.allBusinessGo, src.index)
// 		heap.Fix(&t.allBusinessGo, minTask.index)
// 	}
// 	t.mu.Unlock()
// }

// 重新绑定
// func (t *task) rebindGo(c *Conn) {
// 	if t == c.currBindGo.parent {
// 		t.rebindGoFast(c)
// 		return
// 	}
// 	panic("not support")
// 	// t.rebindGoSlow(c)
// }
