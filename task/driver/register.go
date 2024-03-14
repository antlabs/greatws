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
package driver

import (
	"sync"
)

var (
	driversMu sync.RWMutex
	drivers   = make(map[string]TaskDriver)
)

type TaskPair struct {
	Name   string
	Driver TaskDriver
}

func Register(name string, driver TaskDriver) {
	driversMu.Lock()
	defer driversMu.Unlock()
	if driver == nil {
		panic("greatws.task: Register driver is nil")
	}
	if _, dup := drivers[name]; dup {
		panic("greatws.task: Register called twice for driver " + name)
	}
	drivers[name] = driver
}

func GetAllRegister() []TaskPair {
	rv := make([]TaskPair, 0, len(drivers))

	driversMu.RLock()
	defer driversMu.RUnlock()
	for name, driver := range drivers {
		rv = append(rv, TaskPair{
			Name:   name,
			Driver: driver,
		})
	}
	return rv
}
func GetRegister(name string) TaskDriver {

	driversMu.RLock()
	driver, ok := drivers[name]
	if !ok {
		// 没有找到
		panic("greatws.task: unknown driver " + name)
	}
	defer driversMu.RUnlock()

	return driver
}
