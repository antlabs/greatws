package bigws

import (
	"sync"

	"github.com/antlabs/wsutil/enum"
)

func init() {
	for i := 1; i <= maxIndex; i++ {
		j := i
		pools = append(pools, sync.Pool{
			New: func() interface{} {
				buf := make([]byte, j*page+enum.MaxFrameHeaderSize)
				return &buf
			},
		})
	}
}

const (
	page     = 1024
	maxIndex = 64
)

var pools = make([]sync.Pool, 0, maxIndex)

func selectIndex(n int) int {
	index := n / page
	return index
}

func GetPayloadBytes(n int) (rv *[]byte) {
	index := selectIndex(n - 1)
	if index >= len(pools) {
		rv := make([]byte, n)
		return &rv
	}

	return pools[index].Get().(*[]byte)
}

func PutPayloadBytes(bytes *[]byte) {
	if len(*bytes)%page != 0 {
		return
	}

	newLen := cap(*bytes) - 1
	index := selectIndex(newLen)
	if index >= len(pools) {
		return
	}
	pools[index].Put(bytes)
}