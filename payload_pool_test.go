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
