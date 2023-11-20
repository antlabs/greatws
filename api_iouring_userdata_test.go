package greatws

import "testing"

type testUserDataEncodeDecode struct {
	fd       uint32
	op       ioUringOpState
	writeSeq uint32
}

func Test_UserDataEncodeDecode(t *testing.T) {
	for _, v := range []testUserDataEncodeDecode{
		{1, opRead, 0},
		{1, opWrite, 1},
		{1, opClose, 2},
		{2, opRead, 3},
		{2, opWrite, 4},
		{2, opClose, 5},
		{4, opRead, 6},
		{4, opWrite, 7},
		{4, opClose, 8},
	} {
		userData := encodeUserData(v.fd, v.op, v.writeSeq)
		fd, op, writeSeq := decodeUserData(userData)
		if fd != v.fd || op != v.op || writeSeq != v.writeSeq {
			t.Errorf("encodeUserData(%d, %d, %d) = %d, decodeUserData(%d) = (%d, %d, %d)", v.fd, v.op, v.writeSeq, userData, userData, fd, op, writeSeq)
		}
	}
}
