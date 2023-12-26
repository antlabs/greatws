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
	"reflect"
	"testing"
	"unsafe"
)

func Test_Pool(t *testing.T) {
	t.Run("<=3000", func(t *testing.T) {
		for i := 0; i <= 3000; i++ {
			if i >= 0 && i < 2*pageSize {
				n := putSelectIndex(i)
				if n != 0 {
					t.Fatalf("selectIndex error:(%d):(%d):(%d)\n", 0, n, i)
				}
			} else if i >= 2*pageSize && i < 3*pageSize {
				n := putSelectIndex(i)
				if n != 1 {
					t.Fatalf("selectIndex error:%d:pool-index(%d):%d\n", 1, n, i)
				}

			} else {
				panic(fmt.Sprintf("selectIndex error:%d", i))
			}
		}
	})
}

func getData(s []byte) uintptr {
	return (*reflect.SliceHeader)(unsafe.Pointer(&s)).Data
}

func Test_Index(t *testing.T) {
	for i := 0; i < 1024*128; i++ {
		// fmt.Printf("%d:%d:%d\n", i, getSelectIndex(i), putSelectIndex(i))
	}
}

func Test_PutGet(t *testing.T) {
	t.Run("1024", func(t *testing.T) {
		buf := GetPayloadBytes(1024)
		if len(*buf) != 1024 {
			t.Fatalf("GetPayloadBytes error:%d\n", len(*buf))
		}

		PutPayloadBytes(buf)
		buf2 := GetPayloadBytes(1024)
		if getData(*buf) != getData(*buf2) {
			t.Fatalf("PutPayloadBytes error:%p:%p\n", buf, buf2)
		}
	})

	t.Run("65535", func(t *testing.T) {
		GetPayloadBytes(65535)
	})
}
