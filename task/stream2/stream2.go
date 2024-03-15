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

package stream2

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/antlabs/greatws/task/driver"
)

func init() {
	driver.Register("stream2", &stream2{})
}

var _ driver.TaskDriver = (*stream2)(nil)
var _ driver.Tasker = (*stream2)(nil)
var _ driver.TaskExecutor = (*stream2Executor)(nil)

type stream2 struct {
	initCount  int
	min        int
	max        int
	goroutines int32
	fn         chan func() bool //数据
	haveData   chan struct{}    //控制
	ctx        context.Context
	conf       *driver.Conf
}

func (s *stream2) New(ctx context.Context, initCount, min, max int, c *driver.Conf) driver.Tasker {
	s2 := &stream2{initCount: initCount,
		min:      min,
		max:      max,
		fn:       make(chan func() bool, max),
		haveData: make(chan struct{}, max),
		ctx:      ctx,
		conf:     c,
	}
	go s2.loop()
	return s2
}

func (s *stream2) loop2() {
	defer func() {
		atomic.AddInt32(&s.goroutines, -1)
	}()

	for {
		select {
		case f := <-s.fn:
			if f() {
				return
			}
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *stream2) loop() {
	timeout := time.Second * 10
	tm := time.NewTimer(time.Hour)
	for {
		select {
		case <-s.haveData:
			if atomic.LoadInt32(&s.goroutines) < int32(s.max) {
				atomic.AddInt32(&s.goroutines, 1)
				go s.loop2()
			}
			tm.Reset(timeout)
		case <-tm.C:
			// 10s 没有数据过来，清一波go程
			currGo := atomic.LoadInt32(&s.goroutines)
			if currGo > int32(s.min) {
				need := int((float64(currGo) - float64(s.min)) * 0.1)
				for i := 0; i < need; i++ {
					s.fn <- func() (exit bool) {
						return true
					}
				}
			}
			tm.Reset(timeout)
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *stream2) NewExecutor() driver.TaskExecutor {
	return &stream2Executor{parent: s, list: make([]func() bool, 0, 4)}
}

func (s *stream2) GetGoroutines() int {
	return int(atomic.LoadInt32(&s.goroutines))
}
