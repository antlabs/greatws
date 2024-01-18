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
package greatws

import (
	"container/heap"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// 运行task的策略
// 1. 随机
// 2. 取余映射
type taskStrategy int

const (
	// 送入全局队列，存在的意义主要是用于测试
	taskStrategyRandom taskStrategy = iota
	// 绑定映射, 从一个go程中取一个conn绑定，后面的请求都会在这个go程中处理
	taskStrategyBind
	// 流式映射，一个conntion绑定一个go程(独占)
	taskStrategyStream
)

var ErrTaskQueueFull = errors.New("task queue full")

var exitFunc = func() bool { return true }

type taskConfig struct {
	initCount int // 初始化的协程数
	min       int // 最小协程数
	max       int // 最大协程数
}

// task 模式
// 1. tps模式
// 2. 流量模式
type taskMode int

const (
	tpsMode taskMode = iota
	trafficMode
)

type task struct {
	public  chan func() bool
	windows windows // 滑动窗口计数，用于判断是否需要新增go程
	taskConfig
	curGo   int64 // 当前运行协程数
	curTask int64 // 当前运行任务数

	mu            sync.Mutex
	allBusinessGo allBusinessGo
	id            uint32
	// 窃取id
	stealID         uint32
	taskMode        taskMode
	businessChanNum int
}

func (t *task) nextStealID() uint32 {
	return (atomic.AddUint32(&t.stealID, 1) - 1) % uint32(len(t.allBusinessGo))
}

// func (t *task) nextID() uint32 {
// 	return (atomic.AddUint32(&t.id, 1) - 1) % uint32(len(t.allBusinessGo))
// }

// 初始化
func (t *task) initInner() {
	t.public = make(chan func() bool, runtime.NumCPU())
	t.allBusinessGo = make([]*businessGo, 0, t.initCount)
	t.windows.init()
	go t.manageGo()
	go t.runConsumerLoop()
}

func (t *task) init() {
	t.businessChanNum = runtime.NumCPU()
	if t.taskMode == trafficMode {
		t.businessChanNum = 1024
	}
	t.initInner()
}

// 消费者循环
func (t *task) consumer(steal *businessGo) {
	defer atomic.AddInt64(&t.curGo, -1)
	currBusinessGo := newBusinessGo(t.businessChanNum)
	t.mu.Lock()
	// t.allBusinessGo = append(t.allBusinessGo, currBusinessGo)
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
func (t *task) getGo() *businessGo {
	t.mu.Lock()
	defer t.mu.Unlock()
	v := t.allBusinessGo[0]
	v.addBinConnCount()
	heap.Fix(&t.allBusinessGo, v.index)
	return v
}

func (t *task) addTask(c *Conn, ts taskStrategy, f func() bool) error {

	if ts == taskStrategyBind {
		if c.getCurrBindGo() == nil {
			c.setCurrBindGo(t.getGo())
		}
		currChan := c.getCurrBindGo().taskChan
		// 如果任务未满，直接放入任务队列
		if len(currChan) < cap(currChan) {
			currChan <- f
			return nil
		}

	}
	// 如果任务未满，直接放入公共队列
	if len(t.public) >= cap(t.public) {
		return ErrTaskQueueFull
	}
	t.public <- f
	return nil
}

// 重新绑定
func (t *task) rebindGo(c *Conn) {

	t.mu.Lock()
	minTask := t.allBusinessGo[0]
	src := c.getCurrBindGo()
	if src.bindConnCount > minTask.bindConnCount {
		src.subBinConnCount()
		minTask.addBinConnCount()
		c.setCurrBindGo(minTask)
		heap.Fix(&t.allBusinessGo, src.index)
		heap.Fix(&t.allBusinessGo, minTask.index)
	}
	t.mu.Unlock()
}

func (t *task) addGoWithSteal(g *businessGo) {
	if atomic.LoadInt64(&t.curGo) >= int64(t.max) {
		return
	}
	atomic.AddInt64(&t.curGo, 1)
	go func() {
		defer atomic.AddInt64(&t.curGo, -1)
		t.consumer(g)
	}()
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
	avg := t.windows.avg()
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

	need := (float64(curTask)/float64(curGo)) < 0.25 && curGo < int64(t.windows.avg())
	return need, int(float64(t.curGo) * 0.75)
}

// 管理go程
func (t *task) manageGo() {

	for {
		time.Sleep(time.Second * 1)
		// 当前运行的go程数
		curGo := atomic.LoadInt64(&t.curGo)
		// 记录当前运行的任务数
		t.windows.add(curGo)

		// 1分钟内不考虑收缩go程
		if need, shrinkSize := t.needShrink(); need {
			t.cancelGoNum(shrinkSize)
		} else if need, newSize := t.needGrow(); need {
			t.addGoNum(newSize - int(curGo))
		}
	}

}

// 运行任务
func (t *task) runConsumerLoop() {
	atomic.AddInt64(&t.curGo, int64(t.initCount))
	for i := 0; i < t.initCount; i++ {
		go t.consumer(nil)
	}
}

func (t *task) getCurGo() int64 {
	return atomic.LoadInt64(&t.curGo)
}

func (t *task) getCurTask() int64 {
	return atomic.LoadInt64(&t.curTask)
}
