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
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// 运行task的策略
// 1. 随机
// 2. 取余映射
type taskStrategy int

const (
	// 随机。默认
	taskStrategyRandom taskStrategy = iota
	// 绑定映射
	taskStrategyBind
	// 流式映射
	taskStrategyStream
)

var ErrTaskQueueFull = errors.New("task queue full")

var exitFunc = func() bool { return true }

type taskConfig struct {
	initCount int // 初始化的协程数
	min       int // 最小协程数
	max       int // 最大协程数
}

type task struct {
	c       chan func() bool
	windows windows // 滑动窗口计数，用于判断是否需要新增go程
	taskConfig
	curGo   int64 // 当前运行协程数
	curTask int64 // 当前运行任务数

	allBusinessGo sync.Map // key是[*businessGo, struct{}]， 目前先这样。后面再优化下
}

// 初始化
func (t *task) initInner() {
	t.c = make(chan func() bool)
	t.windows.init()
	go t.manageGo()
	go t.runConsumerLoop()
}

func (t *task) init() {
	t.initInner()
}

func (t *task) getCurGo() int64 {
	return atomic.LoadInt64(&t.curGo)
}

func (t *task) getCurTask() int64 {
	return atomic.LoadInt64(&t.curTask)
}

// 消费者循环
func (t *task) consumer() {
	defer atomic.AddInt64(&t.curGo, -1)
	currBusinessGo := newBusinessGo()
	t.allBusinessGo.Store(currBusinessGo, struct{}{})

	var f func() bool
	for {
		select {
		case f = <-t.c:
		case f = <-currBusinessGo.taskChan:
		}
		atomic.AddInt64(&t.curTask, 1)
		if b := f(); b {
			atomic.AddInt64(&t.curTask, -1)
			if !currBusinessGo.canKill() {
				continue
			}

			break
		}
		atomic.AddInt64(&t.curTask, -1)
	}
}

// 随机获取一个go程
func (t *task) randomGo() *businessGo {
	var currBusinessGo *businessGo
	t.allBusinessGo.Range(func(key, value interface{}) bool {
		currBusinessGo = key.(*businessGo)
		return false
	})
	return currBusinessGo
}

func (t *task) addTask(c *Conn, ts taskStrategy, f func() bool) error {

	if ts == taskStrategyBind {
		if c.currBindGo == nil {
			c.currBindGo = t.randomGo()
			c.currBindGo.addBinConnCount()
		}
		currChan := c.currBindGo.taskChan
		// 如果任务未满，直接放入任务队列
		if len(currChan) < cap(currChan) {
			currChan <- f
			return nil
		}

	}

	if len(t.c) >= cap(t.c) {
		return ErrTaskQueueFull
	}
	t.c <- f
	return nil
}

// 新增go程
func (t *task) addGo() {
	atomic.AddInt64(&t.curGo, 1)
	defer atomic.AddInt64(&t.curGo, -1)
	t.consumer()
}

func (t *task) addGoNum(n int) {
	for i := 0; i < n; i++ {
		go t.addGo()
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
		t.c <- exitFunc
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
		go t.consumer()
	}
}
