// Copyright 2023-2024 antlabs. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package driver

import (
	"context"
	"sync"
)

// 初始化一个go程池
type TaskDriver interface {
	New(ctx context.Context, initCount, min, max int, c *Conf) Tasker
}

// 某个池的实例
type Tasker interface {
	GetGoroutines() int        // 获取goroutine数
	NewExecutor() TaskExecutor // 生成一个扫行器
}

// 处理任务的节点
type TaskExecutor interface {
	AddTask(mu *sync.Mutex, f func() bool) error
	Close(mu *sync.Mutex) error
}
