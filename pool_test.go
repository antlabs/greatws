package bigws

import "testing"

func Test_Pool(t *testing.T) {
	// TODO
	for i := 0; i <= 3000; i++ {
		if i >= 0 && i <= 1024 {
			n := selectIndex(i - 1)
			if n != 0 {
				t.Fatalf("selectIndex error:(%d):(%d):(%d)\n", 0, n, i)
			}
		} else if i > 1024 && i <= 2048 {
			n := selectIndex(i - 1)
			if n != 1 {
				t.Fatalf("selectIndex error:%d:%d:%d\n", 1, n, i)
			}

		} else if i > 2048 && i <= 3072 {
			n := selectIndex(i - 1)
			if n != 2 {
				t.Fatalf("selectIndex error:%d:%d:%d\n", 2, n, i)
			}
		}
	}
}

func Test_PutGet(t *testing.T) {
	buf := GetPayloadBytes(1024)
	if len(*buf) != 1024 {
		t.Fatalf("GetPayloadBytes error:%d\n", len(*buf))
	}

	PutPayloadBytes(buf)
}
