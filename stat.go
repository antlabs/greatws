package greatws

import "sync/atomic"

// 统计信息
type stat struct {
	readSyssall  int64  // 读系统调用次数
	writeSyscall int64  // 写系统调用次数
	curConn      int64  // 当前tcp连接数
	realloc      int64  // 重新分配内存次数
	moveBytes    uint64 // 移动字节数
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

// 对外接口，查询当前连接数
func (m *MultiEventLoop) GetCurConnNum() int64 {
	return atomic.LoadInt64(&m.curConn)
}

// 对外接口，查询当前任务数
func (m *MultiEventLoop) GetCurTaskNum() int64 {
	return m.t.getCurTask()
}

// 对外接口，查询当前任务数
func (m *MultiEventLoop) GetApiName() string {
	if len(m.loops) == 0 {
		return ""
	}

	return m.loops[0].GetApiName()
}

func (m *MultiEventLoop) addRealloc() {
	atomic.AddInt64(&m.realloc, 1)
}

func (m *MultiEventLoop) addReadSyscall() {
	atomic.AddInt64(&m.readSyssall, 1)
}

func (m *MultiEventLoop) addWriteSyscall() {
	atomic.AddInt64(&m.writeSyscall, 1)
}

func (m *MultiEventLoop) addMoveBytes(n uint64) {
	atomic.AddUint64(&m.moveBytes, n)
}
