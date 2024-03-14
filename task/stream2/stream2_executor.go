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
	"runtime"
	"sync"
	"sync/atomic"
	"unsafe"
)

type stream2Executor struct {
	list   []func() bool
	mu     sync.Mutex
	closed uint32
	parent *stream2
}

func (s *stream2Executor) AddTask(f func() bool) error {
	if s.isClose() {
		return nil
	}

	s.mu.Lock()
	if s.isClose() {
		s.mu.Unlock()
		return nil
	}

	process := len(s.list) == 0
	s.list = append(s.list, f)
	s.mu.Unlock()

	if process {
		s.parent.fn <- s.run
	}

	return nil
}

func (s *stream2Executor) run() bool {
	var f func() bool
	for i := 0; ; i++ {
		s.mu.Lock()

		if len(s.list) == i {
			s.list = s.list[0:0]
			s.mu.Unlock()
			return false
		}

		f = s.list[i]
		s.list[i] = nil
		s.mu.Unlock()

		func() {
			defer func() {
				if err := recover(); err != nil {
					const size = 64 << 10
					buf := make([]byte, size)
					buf = buf[:runtime.Stack(buf, false)]
					s.parent.conf.Log.Error("found panic", "err", err, "stack", *(*string)(unsafe.Pointer(&buf)))
				}
			}()
			f()
		}()
	}
}

func (s *stream2Executor) isClose() bool {
	return atomic.LoadUint32(&s.closed) == 1
}

func (s *stream2Executor) Close() error {
	atomic.StoreUint32(&s.closed, 1)
	return nil
}
