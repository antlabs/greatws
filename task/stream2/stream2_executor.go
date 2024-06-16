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
	"unsafe"
)

type stream2Executor struct {
	list   []func() bool
	parent *stream2
}

func myLock(mu *sync.Mutex) {
	if mu != nil {
		mu.Lock()
	}
}

func myUnlock(mu *sync.Mutex) {
	if mu != nil {
		mu.Unlock()
	}
}

func (s *stream2Executor) AddTask(mu *sync.Mutex, f func() bool) error {

	myLock(mu)

	process := len(s.list) == 0
	s.list = append(s.list, f)
	myUnlock(mu)

	if process {
		myLock(mu)
		listSize := len(s.list)
		myUnlock(mu)
		s.parent.fn <- func() bool {
			s.run(mu)
			return false
		}

		s.parent.addOnMessageCount(listSize)

		if len(s.parent.haveData) < cap(s.parent.haveData) {
			select {
			case s.parent.haveData <- struct{}{}:
			default:
			}
		}
	}

	return nil
}

func (s *stream2Executor) run(mu *sync.Mutex) bool {
	var f func() bool
	for i := 0; ; i++ {
		myLock(mu)

		if len(s.list) == 0 {
			myUnlock(mu)
			return false
		}

		if len(s.list) == i {
			s.list = s.list[0:0]
			myUnlock(mu)
			return false
		}

		if i >= len(s.list) {
			s.list = s.list[0:0]
			myUnlock(mu)
			return false
		}

		f = s.list[i]
		s.list[i] = nil
		myUnlock(mu)

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
			s.parent.subOnMessageCount(-1)
		}()
	}
}

func (s *stream2Executor) Close(mu *sync.Mutex) error {
	myLock(mu)

	s.parent.subOnMessageCount(-len(s.list))

	s.list = nil
	myUnlock(mu)
	return nil
}
