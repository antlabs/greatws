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

import "errors"

var (
	OK  = errors.New("OK")
	ERR = errors.New("ERR")
)

type Action int8

const (
	NONE     Action = 0
	READABLE        = 1
	WRITABLE        = 2
	BARRIER         = 4
)

type Event int8

const (
	FILE_EVENTS Event = 1 << iota
	TIME_EVENTS
	DONT_WAIT
	CALL_BEFORE_SLEEP
	CALL_AFTER_SLEEP
	ALL_EVENTS = (FILE_EVENTS | TIME_EVENTS)
)

type IDType int8

const (
	NOMORE           = -1
	DELETED_EVENT_ID = -1
)
