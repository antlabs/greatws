// Copyright 2021-2023 antlabs. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bigws

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/antlabs/wsutil/bytespool"
	"golang.org/x/sys/unix"
)

type UpgradeServer struct {
	config Config
}

func NewUpgrade(opts ...ServerOption) *UpgradeServer {
	var conf ConnOption
	conf.defaultSetting()
	for _, o := range opts {
		o(&conf)
	}
	return &UpgradeServer{config: conf.Config}
}

func (u *UpgradeServer) Upgrade(w http.ResponseWriter, r *http.Request) (c *Conn, err error) {
	return upgradeInner(w, r, &u.config)
}

func Upgrade(w http.ResponseWriter, r *http.Request, opts ...ServerOption) (c *Conn, err error) {
	var conf ConnOption
	conf.defaultSetting()
	for _, o := range opts {
		o(&conf)
	}
	return upgradeInner(w, r, &conf.Config)
}

func getFdFromConn(c net.Conn) (fd int, err error) {
	var TCPConn *net.TCPConn
	var ok bool
	TCPConn, ok = c.(*net.TCPConn)
	if !ok {
		return 0, errors.New("not tcp conn")
	}
	file, err := TCPConn.File()
	if err != nil {
		return 0, err
	}
	fd = int(file.Fd())
	err = unix.SetNonblock(fd, true)
	if err != nil {
		unix.Close(fd)
		fd = 0
	}
	return
}

func newConn(fd int, client bool, conf *Config) *Conn {
	c := &Conn{
		conn: conn{
			fd:   fd,
			wbuf: make([]byte, 1024),
		},
		Config: conf,
		client: client,
	}
	return c
}

func upgradeInner(w http.ResponseWriter, r *http.Request, conf *Config) (c *Conn, err error) {
	if ecode, err := checkRequest(r); err != nil {
		http.Error(w, err.Error(), ecode)
		return nil, err
	}

	hi, ok := w.(http.Hijacker)
	if !ok {
		return nil, ErrNotFoundHijacker
	}

	// var read *bufio.Reader
	var conn net.Conn
	conn, _, err = hi.Hijack()
	if err != nil {
		return nil, err
	}

	fd, err := getFdFromConn(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	// 已经dup了一份fd，所以这里可以关闭
	conn.Close()

	c = newConn(fd, false, conf)
	conf.multiEventLoop.add(c)

	// TODO
	// var rw *bufio.ReadWriter
	// if conf.parseMode == ParseModeWindows {
	// 	// 这里不需要rw，直接使用conn
	// 	conn, rw, err = hi.Hijack()
	// 	if !conf.disableBufioClearHack {
	// 		bufio2.ClearReadWriter(rw)
	// 	}
	// 	// TODO
	// 	// rsp.ClearRsp(w)
	// 	rw = nil
	// } else {
	// 	var rw *bufio.ReadWriter
	// 	conn, rw, err = hi.Hijack()
	// 	read = rw.Reader
	// 	rw = nil
	// }
	// if err != nil {
	// 	return nil, err
	// }

	// 是否打开解压缩
	// 外层接收压缩, 并且客户端发送扩展过来
	if conf.decompression {
		conf.decompression = needDecompression(r.Header)
	}

	buf := bytespool.GetUpgradeRespBytes()

	tmpWriter := bytes.NewBuffer((*buf)[:0])
	defer func() {
		bytespool.PutUpgradeRespBytes(buf)
		tmpWriter = nil
	}()
	if err = prepareWriteResponse(r, tmpWriter, conf); err != nil {
		return
	}

	if _, err := conn.Write(tmpWriter.Bytes()); err != nil {
		return nil, err
	}

	// var fr fixedreader.FixedReader
	// var bp bytespool.BytesPool
	// bp.Init()
	// if conf.parseMode == ParseModeWindows {
	// 	fr.Init(conn, bytespool.GetBytes(conf.initPayloadSize()))
	// }

	conn.SetDeadline(time.Time{})
	// return newConn(conn, false, conf, fr, read, bp), nil
	return nil, nil
}
