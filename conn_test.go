package greatws

import (
	"testing"
	"unsafe"
)

func Test_Conn(t *testing.T) {
	t.Run("conn size", func(t *testing.T) {
		// 在未加入tls功能时，Conn的大小为160字节够用了。
		if unsafe.Sizeof(Conn{}) > 160 {
			t.Errorf("Conn size is too large")
		}
	})

}
