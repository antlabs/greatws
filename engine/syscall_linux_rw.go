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

//go:build linux && fio_rwsyscall

package engine

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// Linux 默认实现: read(2)/write(2)。
//
// 与 x/sys/unix 的 Read/Write 的区别在于这里用 RawSyscall 而不是 Syscall。
// 标准库的 syscall.Syscall 是这样包装的:
//
//	func Syscall(trap, ...) {
//	    runtime_entersyscall()      // 标记 goroutine 进入系统调用
//	    RawSyscall(...)
//	    runtime_exitsyscall()       // 回来重新抢 P
//	}
//
// entersyscall/exitsyscall 要解除与恢复 P 的绑定、检查抢占、必要时触发调度。
// 在每次 echo 至少两次 syscall(一次读一次写)的负载下, 这份开销摊在每一次
// 收发上。fib 直接调 RawSyscall6 绕开它, pprof 里 fib 的 syscall 路径占了
// 总数的 75%, 而这条路径上的差异就是主要的归因方向。
//
// 用 RawSyscall 的前提是 syscall 不会阻塞: 一旦某个 syscall 阻塞在内核里,
// 占住的 P 无法被调度给其他 goroutine, liveness 会受影响。这里成立, 因为:
//   - fd 是非阻塞的。fio 的连接来自 http.Server 的 Hijack,
//     Go 标准库 accept 出来的 fd 自带 O_NONBLOCK(实测 flags=0x6);
//     unix.Dup 复制 fd 时共享同一份 file description, 非阻塞属性一并继承
//   - 调用点都在 epoll 报告可读/可写之后, 正常路径下立即返回
//   - 返回 EAGAIN 时按"暂时无数据"处理, 不会原地重试等待
//
// 保留 unix.EINTR 之类的错误码语义: RawSyscall 返回的 errno 与 Syscall 相同。

func socketRead(fd int, p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	r, _, errno := unix.RawSyscall6(unix.SYS_READ,
		uintptr(fd), uintptr(unsafe.Pointer(&p[0])), uintptr(len(p)),
		0, 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(r), nil
}

func socketWrite(fd int, p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	r, _, errno := unix.RawSyscall6(unix.SYS_WRITE,
		uintptr(fd), uintptr(unsafe.Pointer(&p[0])), uintptr(len(p)),
		0, 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return int(r), nil
}

// socketWritev / wsHeader 的兜底: 这些构建组合下没有 sendmsg 快路径,
// 退回"拼成一块再写"。
func socketWritev(fd int, header, payload []byte) (int, error) {
	all := make([]byte, 0, len(header)+len(payload))
	all = append(all, header...)
	all = append(all, payload...)
	return socketWrite(fd, all)
}
