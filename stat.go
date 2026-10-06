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

import "sync/atomic"

// 统计信息
type stat struct {
	readSyssall  int64  // 读系统调用次数
	writeSyscall int64  // 写系统调用次数
	curConn      int64  // 当前tcp连接数
	realloc      int64  // 重新分配内存次数
	moveBytes    uint64 // 移动字节数
	readEv       int64  // 读事件次数
	writeEv      int64  // 写事件次数
	pollEv       int64  // poll事件次数, 包含读,写, 错误事件
}

// 对外接口，查询当前业务协程池个数
func (m *MultiEventLoop) GetCurGoNum() (total int) {
	for _, v := range m.loops {
		// 本地任务数
		total += int(v.localTask.GetGoroutines())
	}
	return
}

// 对外接口，查询业务协程池运行的当前业务数
func (m *MultiEventLoop) GetCurTaskNum() (total int64) {
	for _, v := range m.loops {
		// 本地任务数
		total += int64(v.localTask.GetGoroutines())
	}
	return
}

// 对外接口，查询移动字节数
func (m *MultiEventLoop) GetMoveBytesNum() uint64 {
	return atomic.LoadUint64(&m.moveBytes)
}

// 对外接口，查询重新分配内存次数
func (m *MultiEventLoop) GetReallocNum() int64 {
	return atomic.LoadInt64(&m.realloc)
}

// 对外接口，查询read syscall次数
func (m *MultiEventLoop) GetReadSyscallNum() int64 {
	return atomic.LoadInt64(&m.readSyssall)
}

// 对外接口，查询write syscall次数
func (m *MultiEventLoop) GetWriteSyscallNum() int64 {
	return atomic.LoadInt64(&m.writeSyscall)
}

// 对外接口，查询当前websocket连接数
func (m *MultiEventLoop) GetCurConnNum() int64 {
	return atomic.LoadInt64(&m.curConn)
}

// 对外接口，查询poll read事件次数
func (m *MultiEventLoop) GetReadEvNum() int64 {
	return atomic.LoadInt64(&m.readEv)
}

// 对外接口，查询poll write事件次数
func (m *MultiEventLoop) GetWriteEvNum() int64 {
	return atomic.LoadInt64(&m.writeEv)
}

// 对外接口，查询poll 返回的事件总次数
func (m *MultiEventLoop) GetPollEvNum() int64 {
	return atomic.LoadInt64(&m.pollEv)
}

// 对外接口，返回当前使用的api名字
func (m *MultiEventLoop) GetApiName() string {
	if len(m.loops) == 0 {
		return ""
	}

	return m.loops[0].GetApiName()
}

// 对内接口
func (m *MultiEventLoop) addRealloc() {
	atomic.AddInt64(&m.realloc, 1)
}

// 系统调用计数。就是两个原子加, 没做本地批量——试过按分片本地累加再
// 攒批上报, 但写路径可以从任意 go 程调(用户的 OnMessage、超时线程),
// 本地字段就要跨 go 程写, 反而要加锁; 换来的是 1.7% 的吞吐, 不值。
func (c *Conn) addReadSyscall() {
	atomic.AddInt64(&c.multiEventLoop.readSyssall, 1)
}

func (c *Conn) addWriteSyscall() {
	atomic.AddInt64(&c.multiEventLoop.writeSyscall, 1)
}

// 对内接口
func (m *MultiEventLoop) addMoveBytes(n uint64) {
	atomic.AddUint64(&m.moveBytes, n)
}

// 对内接口
func (m *MultiEventLoop) addReadEvNum() {
	atomic.AddInt64(&m.readEv, 1)
}

// 对内接口
func (m *MultiEventLoop) addWriteEvNum() {
	atomic.AddInt64(&m.writeEv, 1)
}

// 对内接口
func (m *MultiEventLoop) addPollEvNum() {
	atomic.AddInt64(&m.pollEv, 1)
}
