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
	"reflect"
	"testing"
	"unsafe"
)

func Test_Pool(t *testing.T) {
	t.Run("<=1024", func(t *testing.T) {
		for i := 0; i <= 1024; i++ {
			got := getSelectIndex(i)
			if got != 0 {
				t.Fatalf("getSelectIndex error:%d\n", i)
			}
		}
	})
	t.Run("1025-2048", func(t *testing.T) {
		for i := 1025; i <= 2048; i++ {
			got := getSelectIndex(i)
			if got != 1 {
				t.Fatalf("getSelectIndex error:%d\n", i)
			}
		}
	})

	t.Run("2049-3072", func(t *testing.T) {
		for i := 2049; i <= 3072; i++ {
			got := getSelectIndex(i)
			if got != 2 {
				t.Fatalf("getSelectIndex error:%d\n", i)
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
