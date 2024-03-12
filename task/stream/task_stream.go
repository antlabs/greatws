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
	"errors"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/antlabs/greatws/task/driver"
)

func init() {
	driver.Register("stream", &taskStream{})
}

var _ driver.TaskExecutor = (*taskStream)(nil)
var _ driver.TaskDriver = (*taskStream)(nil)
var _ driver.Tasker = (*taskStream)(nil)

type taskStream struct {
	streamChan chan func() bool
	sync.Once
	closed   uint32
	currency int64
}

func (t *taskStream) loop() {
	for cb := range t.streamChan {
		cb()
	}
}

// 这里构造了一个新的实例
func (t *taskStream) New(initCount, min, max int) driver.Tasker {
	return t
}

// 创建一个执行器，由于没有node的概念，这里直接返回自己
func (t *taskStream) NewExecutor() driver.TaskExecutor {
	var t2 taskStream
	t2.init()
	return &t2
}

func (t *taskStream) GetGoroutines() int {
	return int(atomic.LoadInt64(&t.currency))
} // 获取goroutine数

func (t *taskStream) init() {
	t.streamChan = make(chan func() bool, runtime.NumCPU())
	go t.loop()
}

func (t *taskStream) AddTask(f func() bool) (err error) {
	if atomic.LoadUint32(&t.closed) == 1 {
		return nil
	}

	defer func() {
		if e1 := recover(); e1 != nil {
			err = errors.New("found panic in AddTask: " + e1.(string))
			return
		}
	}()
	t.streamChan <- f
	// TODO: 阻塞的情况如何处理?
	// greatws 处理overflow的fd
	return nil
}

func (t *taskStream) Close() error {
	t.Do(func() {
		close(t.streamChan)
		atomic.StoreUint32(&t.closed, 1)
	})

	return nil
}
