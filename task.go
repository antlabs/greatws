package bigws

type task struct {
	c   chan func()
	max int
}

func newTask(max int) *task {
	t := &task{
		c:   make(chan func()),
		max: max,
	}
	go t.run()
	return t
}

func (t *task) runLoop() {
	for f := range t.c {
		f()
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
