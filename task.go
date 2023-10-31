package bigws

import "sync/atomic"

type task struct {
	c       chan func()
	max     int
	curTask int64
}

func newTask(max int) *task {
	t := &task{
		c:   make(chan func()),
		max: max,
	}
	go t.run()
	return t
}

func (t *task) getCurTask() int64 {
	return atomic.LoadInt64(&t.curTask)
}

func (t *task) runLoop() {
	for f := range t.c {
		atomic.AddInt64(&t.curTask, 1)
		f()
		atomic.AddInt64(&t.curTask, -1)
	}
}

func (t *task) addTask(f func()) {
	t.c <- f
}

func (t *task) run() {
	for i := 0; i < t.max; i++ {
		go t.runLoop()
	}
}
