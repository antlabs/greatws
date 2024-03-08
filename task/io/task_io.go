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

import "github.com/antlabs/greatws/task/driver"

func init() {
	driver.Register("io", &taskIo{})
}

var _ driver.TaskExecutor = (*taskIo)(nil)
var _ driver.TaskDriver = (*taskIo)(nil)
var _ driver.Tasker = (*taskIo)(nil)

type taskIo struct{}

func (t *taskIo) GetGoroutines() int { return 0 } // 获取goroutine数

func (t *taskIo) New(initCount, min, max int) driver.Tasker {
	return &taskIo{}
}

func (t *taskIo) NewExecutor() driver.TaskExecutor {
	return &taskIo{}
}

// 任务运行在io goroutine中
func (t *taskIo) AddTask(f func() bool) error {
	f()
	return nil
}

func (t *taskIo) Close() error {
	return nil
}
