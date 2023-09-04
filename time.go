// Copyright 2021-2023 antlabs. All rights reserved.
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
package bigws

import (
	"container/list"
	"time"
)

type TimeEvent struct {
	id            int
	when          time.Time
	timeProc      TimeProc
	finalizerProc EventFinalizerProc
	clientData    interface{}
	refcount      int
}

func createTimeEvent() *list.List {
	return list.New()
}

func addTimeEvent(head *list.List, id int, when time.Duration, proc TimeProc, clientData interface{}, finalizerProc EventFinalizerProc) {
	te := &TimeEvent{
		id:            id,
		when:          time.Now().Add(when),
		timeProc:      proc,
		finalizerProc: finalizerProc,
		clientData:    clientData,
	}

	head.PushFront(te)
}
