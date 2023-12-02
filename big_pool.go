package greatws

import "sync"

// import "sync"

var bigPool = sync.Pool{
	New: func() interface{} {
		buf := make([]byte, 0, 1024*256)
		return &buf
	},
}

func getBigPayload(n int) (rv *[]byte) {
	rv = bigPool.Get().(*[]byte)
	if cap(*rv) < n {
		tmp := make([]byte, n)
		*rv = tmp
	} else {
		*rv = (*rv)[:n]
	}
	return rv
}

func putBigPayload(buf *[]byte) {
	bigPool.Put(buf)
}
