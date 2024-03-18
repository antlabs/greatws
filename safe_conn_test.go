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
	"fmt"
	"testing"
)

func Test_safe_conn(t *testing.T) {
	t.Run("safe_conn_del", func(t *testing.T) {
		fmt.Printf("ptrSize:%d\n", ptrSize)
		var s safeConns
		s.conns = make([]*Conn, 3)
		// 创建
		s.addConn(&Conn{conn: conn{fd: int64(0)}})

		c := s.getConn(0)
		s.delConn(c)

		c = s.getConn(0)

		if c != nil {
			t.Errorf("s.conns:%v\n", c)
		}
	})

	t.Run("safe_conn_add", func(t *testing.T) {
		var s safeConns
		s.conns = make([]*Conn, 3)
		// 创建
		for i := 0; i < 100; i++ {
			s.addConn(&Conn{conn: conn{fd: int64(i)}})
		}

		// 查找
		for i := 0; i < 100; i++ {
			c := s.getConn(i)
			if c != s.conns[i] {
				t.Errorf("s.conns:%v\n", c)
				return
			}

			if c == nil {
				t.Errorf("conn is nil\n")
				return
			}

			if c.fd != int64(i) {
				t.Errorf("s.conns:%v\n", c)
				return
			}
			if c.fd != s.conns[i].fd {
				t.Errorf("s.conns:%v\n", c)
				return
			}
		}

		// 删除
		for i := 0; i < 100; i++ {
			c := s.getConn(i)
			if c.fd != int64(i) {
				t.Errorf("s.conns:%v\n", c)
				return
			}
			s.delConn(c)

			c = s.getConn(i)

			if c != nil {
				t.Errorf("i = %d, s.conns:%p, %p, %p\n", i, c, &s.conns[i], s.conns[i])
				return
			}
		}
	})

	t.Run("safe_conn_add_not_init", func(t *testing.T) {
		var s safeConns
		// 创建
		for i := 0; i < 100; i++ {
			s.addConn(&Conn{conn: conn{fd: int64(i)}})
		}

		// 查找
		for i := 0; i < 100; i++ {
			c := s.getConn(i)
			if c != s.conns[i] {
				t.Errorf("s.conns:%v\n", c)
				return
			}

			if c == nil {
				t.Errorf("conn is nil\n")
				return
			}

			if c.fd != int64(i) {
				t.Errorf("s.conns:%v\n", c)
				return
			}
			if c.fd != s.conns[i].fd {
				t.Errorf("s.conns:%v\n", c)
				return
			}
		}

		// 删除
		for i := 0; i < 100; i++ {
			c := s.getConn(i)
			if c.fd != int64(i) {
				t.Errorf("s.conns:%v\n", c)
				return
			}
			s.delConn(c)

			c = s.getConn(i)

			if c != nil {
				t.Errorf("i = %d, s.conns:%p, %p, %p\n", i, c, &s.conns[i], s.conns[i])
				return
			}
		}
	})
}
