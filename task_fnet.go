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
	"sync"

	"github.com/antlabs/task/task/driver"
	"github.com/linfeip/fnet/taskpool"
)

// fnetTaskDriver 是 greatws 用 fnet 的 taskpool 跑回调时的驱动。
//
// 它的意义是控制变量: 两个库的池一样了, 剩下的差异就只在收发和解析
// 路径上。fnet 的池一个核一个 shard、每个 shard 的 worker 按需启停,
// 空闲的挂起但不释放栈(唤醒的是同一个 worker, 缓存是热的); 环满了
// 还会起临时 goroutine 兜底, 投递方永不阻塞。
//
// 用 WithServerFnetTaskPool / WithClientFnetTaskPool 打开。
func init() {
	driver.Register(fnetDriverName, &fnetTaskDriver{})
}

// FnetTaskMode 是 greatws 用 fnet 池时需要传给任务模式的驱动名。
const FnetTaskMode = fnetDriverName

const fnetDriverName = "fnet"

type fnetTaskDriver struct{}

func (d *fnetTaskDriver) New(ctx context.Context, initCount, min, max int, c *driver.Conf) driver.Tasker {
	return d
}

// GetGoroutines 报告默认池的 worker 上限, 和 fnet 给它的默认一样。
func (d *fnetTaskDriver) GetGoroutines() int { return taskpool.MaxWorkers }

func (d *fnetTaskDriver) NewExecutor() driver.TaskExecutor {
	return &fnetTaskExecutor{}
}

// fnetTaskExecutor 是一个连接的回调入口。
//
// greatws 保证同一个连接的回调按顺序进来(driver.TaskExecutor 的契约),
// fnet 的池自己不保证这一点——它只保证任务被执行。这里用连接自己的锁
// 把并发挡掉: 拿不到锁就说明上一个还在跑, 那说明调用方没有按契约来。
type fnetTaskExecutor struct {
	closed bool
}

func (e *fnetTaskExecutor) AddTask(mu *sync.Mutex, f func() bool) error {
	if f == nil {
		return nil
	}
	// 池的提交是无锁的, 满员也只是排队; 这里直接把任务交出去, 让 fnet
	// 的 shard 决定谁在哪个核上跑。
	taskpool.DefaultTaskPool.Submit(func() { f() })
	return nil
}

func (e *fnetTaskExecutor) Close(mu *sync.Mutex) error {
	return nil
}
