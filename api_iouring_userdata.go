package greatws

// UserData， 低32位存放fd，高32位存放op等控制信息
// 对于写事件来说，低32位存放fd, 高32位的高16位存放write seq。低16位存放op
func encodeUserData(fd uint32, op ioUringOpState, writeSeq uint32) uint64 {
	return uint64(op)<<32 | uint64(fd) | uint64(writeSeq)<<(32+16)
}

func decodeUserData(userData uint64) (fd uint32, op ioUringOpState, writeSeq uint32) {
	fd = uint32(userData & 0xffffffff)
	op = ioUringOpState(userData >> 32 & 0xFFFF)
	writeSeq = uint32(userData >> (32 + 16))
	return
}
