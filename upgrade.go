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
	"syscall"
	"time"

	"github.com/antlabs/wsutil/bufio2"
	"github.com/antlabs/wsutil/bytespool"
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
	conf.Callback = newGoCallback(conf.Callback, conf.multiEventLoop.t)
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
	conf.Callback = newGoCallback(conf.Callback, conf.Config.multiEventLoop.t)
	return upgradeInner(w, r, &conf.Config)
}

func getFdFromConn(c net.Conn) (newFd int, err error) {
	sc, ok := c.(interface {
		SyscallConn() (syscall.RawConn, error)
	})
	if !ok {
		return 0, errors.New("RawConn Unsupported")
	}
	rc, err := sc.SyscallConn()
	if err != nil {
		return 0, errors.New("RawConn Unsupported")
	}

	err = rc.Control(func(fd uintptr) {
		newFd = int(fd)
	})
	if err != nil {
		return 0, err
	}

	return duplicateSocket(int(newFd))
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
	conn, rw, err := hi.Hijack()
	if err != nil {
		return nil, err
	}
	if !conf.disableBufioClearHack {
		bufio2.ClearReadWriter(rw)
	}

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

	if err = conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}

	fd, err := getFdFromConn(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	// 已经dup了一份fd，所以这里可以关闭
	if err = conn.Close(); err != nil {
		return nil, err
	}

	c = newConn(int64(fd), false, conf)
	if err = conf.multiEventLoop.add(c); err != nil {
		return nil, err
	}

	// fmt.Printf("new fd = %d, %p\n", fd, c)

	// return newConn(conn, false, conf, fr, read, bp), nil
	return c, nil
}
