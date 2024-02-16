package greatws

import "runtime"

type taskParse struct {
	allTaskParse []*taskParseNode
}

func newTaskParse() *taskParse {
	tp := &taskParse{
		allTaskParse: make([]*taskParseNode, runtime.NumCPU()),
	}
	tp.start()

	return tp
}

func (t *taskParse) start() {
	for i := 0; i < len(t.allTaskParse); i++ {
		t.allTaskParse[i] = &taskParseNode{
			taskChan: make(chan func() bool, 1024),
		}
		go t.allTaskParse[i].run()
	}
}

func (t *taskParse) addTask(fd int, f func() bool) {
	t.allTaskParse[fd%len(t.allTaskParse)].taskChan <- f
}

type taskParseNode struct {
	taskChan chan func() bool
}

func (tpn *taskParseNode) run() {
	for {
		select {
		case f := <-tpn.taskChan:
			if !f() {
				return
			}
		}
	}
}
