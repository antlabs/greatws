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
	fn         chan func() bool
	ctx        context.Context
	conf       *driver.Conf
}

func (s *stream2) New(ctx context.Context, initCount, min, max int, c *driver.Conf) driver.Tasker {
	s2 := &stream2{initCount: initCount, min: min, max: max, fn: make(chan func() bool, max), ctx: ctx, conf: c}
	go s2.loop()
	return s2
}

func (s *stream2) loop2(f func() bool) {
	if f != nil {
		f()
	}

	defer func() {
		atomic.AddInt32(&s.goroutines, -1)
	}()
	for {
		select {
		case f := <-s.fn:
			f()
		case <-s.ctx.Done():
			return
		default:
			return
		}
	}
}

func (s *stream2) loop() {
	for {
		select {
		case f := <-s.fn:
			if atomic.LoadInt32(&s.goroutines) < int32(s.max) {
				atomic.AddInt32(&s.goroutines, 1)
				go s.loop2(f)
			}
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
