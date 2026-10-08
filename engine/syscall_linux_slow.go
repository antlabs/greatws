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

//go:build linux && fio_slowsyscall

package engine

import "golang.org/x/sys/unix"

// 走标准库 syscall.Syscall 的对照实现, 用 -tags fio_slowsyscall 启用。
//
// 这是 greatws(改名前)历史上的行为, 保留下来只为对照实验: 它和默认实现的唯一
// 区别是用了 x/sys/unix 的 Read/Write, 而后者内部是
//
//	func Syscall(trap, ...) {
//	    runtime_entersyscall()   // 解除 P 绑定、检查抢占
//	    RawSyscall(...)
//	    runtime_exitsyscall()    // 回来重新抢 P
//	}
//
// 比直接 RawSyscall 每次多两个 runtime 调用。pprof 显示 fib 的 syscall
// 路径占总 CPU 的 75%, 且 fib 用的是 RawSyscall6, 所以这里量的是
// "enter/exitsyscall 值多少"。
//
// 注意: 这个标签下 fd 仍然必须是非阻塞的(与默认实现同一前提), 否则
// entersyscall/exitsyscall 也救不回被占住的线程。

func socketRead(fd int, p []byte) (int, error) {
	return unix.Read(fd, p)
}

func socketWrite(fd int, p []byte) (int, error) {
	return unix.Write(fd, p)
}

// socketWritev / socketWritev3 / wsHeader 的兜底: 这些构建组合下没有
// sendmsg 快路径, 退回"拼成一块再写"。
func socketWritev(fd int, header, payload []byte) (int, error) {
	all := make([]byte, 0, len(header)+len(payload))
	all = append(all, header...)
	all = append(all, payload...)
	return socketWrite(fd, all)
}

func socketWritev3(fd int, first, header, payload []byte) (int, error) {
	all := make([]byte, 0, len(first)+len(header)+len(payload))
	all = append(all, first...)
	all = append(all, header...)
	all = append(all, payload...)
	return socketWrite(fd, all)
}
