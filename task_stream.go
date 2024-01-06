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
	"sync"
)

type taskStream struct {
	streamChan chan func() bool
	sync.Once
}

func (t *taskStream) loop() {
	for cb := range t.streamChan {
		cb()
	}
}
func (t *taskStream) init() {
	t.streamChan = make(chan func() bool, 3)
	go t.loop()
}

func (t *taskStream) addTask(ts taskStrategy, f func() bool) {
	defer func() {
		if err := recover(); err != nil {

		}
	}()
	t.streamChan <- f
	// TODO阻塞的情况如何处理?
	// 默认启动oneshot模式
}

func (t *taskStream) close() {
	t.Do(func() {
		close(t.streamChan)
	})
}
