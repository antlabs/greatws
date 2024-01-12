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
package greatws

import "sync/atomic"

type businessGo struct {
	taskChan chan func() bool
	// 被多少conn绑定
	bindConnCount int64
	closed        uint32
}

func (b *businessGo) isClose() bool {
	return atomic.LoadUint32(&b.closed) == 1
}

// 新增绑定的conn数
func (b *businessGo) addBinConnCount() {
	atomic.AddInt64(&b.bindConnCount, 1)
}

// 减少绑定的conn数
func (b *businessGo) subBinConnCount() {
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

func newBusinessGo() *businessGo {
	return &businessGo{
		taskChan: make(chan func() bool, 3),
	}
}
