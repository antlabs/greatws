package greatws

import (
	"sync/atomic"
	"time"
)

var exitFunc = func() bool { return true }

type task struct {
	c chan func() bool

	initCount int   // 初始化的协程数
	min       int   // 最小协程数
	max       int   // 最大协程数
	curGo     int64 // 当前运行协程数
	curTask   int64 // 当前运行任务数
}

func (t *task) init() {
	t.c = make(chan func() bool)
	go t.manageGo()
	go t.runConsumerLoop()
}

func (t *task) getCurTask() int64 {
	return atomic.LoadInt64(&t.curTask)
}

// 消费者循环
func (t *task) consumer() {
	for f := range t.c {
		atomic.AddInt64(&t.curTask, 1)
		if b := f(); b {
			atomic.AddInt64(&t.curTask, -1)
			break
		}
		atomic.AddInt64(&t.curTask, -1)
	}
}

// 新增任务
func (t *task) addTask(f func() bool) {
	t.c <- f
}

// 新增go程
func (t *task) addGo() {
	go func() {
		atomic.AddInt64(&t.curGo, 1)
		defer atomic.AddInt64(&t.curGo, -1)
		t.consumer()
	}()
}

// 取消go程
func (t *task) cancelGo() {
	if atomic.LoadInt64(&t.curGo) > int64(t.min) {
		t.c <- exitFunc
	}
}

// 管理go程
func (t *task) manageGo() {
	for {

		time.Sleep(time.Second * 5)
		curTask := atomic.LoadInt64(&t.curTask)
		curGo := atomic.LoadInt64(&t.curGo)

		if curTask < int64(t.min) && curGo > int64(t.min) {
			t.cancelGo()
		} else if curTask < int64(t.max) && (float64(curTask)/float64(curGo)) > 0.8 {
			t.addGo()
		}
	}
}

// 运行任务
func (t *task) runConsumerLoop() {
	for i := 0; i < t.initCount; i++ {
		go t.consumer()
	}
}
