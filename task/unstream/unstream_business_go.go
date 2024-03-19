// Copyright 2021-2024 antlabs. All rights reserved.
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
package unstream

import (
	"sync"
	"sync/atomic"
	"unsafe"
)

type businessGo struct {
	taskChan chan func() bool
	// 被多少conn绑定
	bindConnCount int64
	index         int // 在min heap中的索引，方便删除或者重新推入堆中
	parent        *task
	closed        uint32
	count         uint32
}

func (b *businessGo) isClose() bool {
	return atomic.LoadUint32(&b.closed) == 1
}

// 新增绑定的conn数
func (b *businessGo) addBinConnCount() {
	atomic.AddInt64(&b.bindConnCount, 1)
}

// 减少绑定的conn数
func (b *businessGo) subBindConnCount() {
	atomic.AddInt64(&b.bindConnCount, -1)
}

// 是否可以杀死这个go程
func (b *businessGo) canKill() bool {
	curConn := atomic.LoadInt64(&b.bindConnCount)
	if curConn < 0 {
		panic("current conn  < 0")
	}
	return curConn == 0
}

func newBusinessGo(num int, parent *task) *businessGo {
	return &businessGo{
		taskChan: make(chan func() bool, num),
		parent:   parent,
	}
}

func (t *businessGo) AddTask(mu *sync.Mutex, f func() bool) error {

	newCount := atomic.AddUint32(&t.count, 1)
	// 如果任务未满，直接放入任务队列
	if len(t.taskChan) < cap(t.taskChan) {
		t.taskChan <- f
		return nil
	}

	node := t.parent.getGoBusiness(uintptr(unsafe.Pointer(t)))
	if len(node.taskChan) < cap(node.taskChan) {
		node.taskChan <- f
		return nil
	}

	if newCount%63 == 0 && len(t.parent.public) < cap(t.parent.public) {
		t.parent.public <- f
		return nil
	}
	// 判断go程是否增长，如果增长，重新映射
	t.taskChan <- f
	return nil
}

// 一些统计状态和资源的关闭
func (t *businessGo) Close(mu *sync.Mutex) error {
	if t.isClose() {
		return nil
	}
	t.subBindConnCount()
	atomic.StoreUint32(&t.closed, 1)
	return nil
}

type allBusinessGo []*businessGo

func (a allBusinessGo) Less(i, j int) bool {
	return atomic.LoadInt64(&a[i].bindConnCount) < atomic.LoadInt64(&a[j].bindConnCount)
}

func (a allBusinessGo) Len() int { return len(a) }

func (a allBusinessGo) Swap(i, j int) {
	a[i], a[j] = a[j], a[i]
	a[i].index = i
	a[j].index = j
}

func (a *allBusinessGo) Push(x any) {
	// Push and Pop use pointer receivers because they modify the slice's length,
	// not just its contents.
	*a = append(*a, x.(*businessGo))
	lastIndex := len(*a) - 1
	(*a)[lastIndex].index = lastIndex
}

func (a *allBusinessGo) Pop() any {
	old := *a
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	item.index = -1
	*a = old[0 : n-1]
	return item
}
