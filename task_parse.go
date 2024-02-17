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
)

type taskParse struct {
	allTaskParse []*taskParseNode
}

func newTaskParse() *taskParse {
	tp := &taskParse{
		allTaskParse: make([]*taskParseNode, runtime.NumCPU()),
	}
	wg := sync.WaitGroup{}
	wg.Add(runtime.NumCPU())

	tp.start(&wg)
	wg.Wait()
	return tp
}

func (t *taskParse) start(wg *sync.WaitGroup) {
	for i := 0; i < len(t.allTaskParse); i++ {
		t.allTaskParse[i] = &taskParseNode{
			taskChan: make(chan func() bool, 1024),
		}
		go t.allTaskParse[i].run(wg)
	}
}

func (t *taskParse) addTask(fd int, f func() bool) {
	t.allTaskParse[fd%len(t.allTaskParse)].taskChan <- f
}

type taskParseNode struct {
	taskChan chan func() bool
}

func (tpn *taskParseNode) run(wg *sync.WaitGroup) {
	wg.Done()
	for f := range tpn.taskChan {
		if !f() {
			return
		}
	}
}
