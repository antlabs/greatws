package greatws

import "sync"

var fragmentPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 0, 1024)
		return &buf
	},
}

func GetFragmentBytes() *[]byte {
	buf := fragmentPool.Get().(*[]byte)
	*buf = (*buf)[:0]
	return buf
}

func PutFragmentBytes(buf *[]byte) {
	fragmentPool.Put(buf)
}
