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
	"context"

	"github.com/antlabs/task/task/driver"
)

type selectTask struct {
	taskDriverName string
	task           driver.Tasker
}
type selectTasks []selectTask

func newSelectTask(ctx context.Context, initCount, min, max int, c *driver.Conf) []selectTask {

	all := driver.GetAllRegister()
	rv := make([]selectTask, 0, len(all))
	for _, val := range all {
		task := val.Driver.New(ctx, initCount, min, max, c)
		rv = append(rv, selectTask{
			taskDriverName: val.Name,
			task:           task,
		})
	}
	return rv
}

func (s *selectTasks) newTask(taskName string) driver.TaskExecutor {
	for _, val := range *s {
		if val.taskDriverName == taskName {
			return val.task.NewExecutor()
		}
	}

	panic("greatws: no task driver found:" + taskName)
}

func (s *selectTasks) GetGoroutines() int {
	total := 0
	for _, val := range *s {
		total += val.task.GetGoroutines()
	}

	return total
}
