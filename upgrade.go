// Copyright 2023-2024 antlabs. All rights reserved.
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

package greatws

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"syscall"
	"time"

	"github.com/antlabs/wsutil/bytespool"
	"github.com/antlabs/wsutil/deflate"
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
	conf.defaultSettingAfter()
	return &UpgradeServer{config: conf.Config}
}

func (u *UpgradeServer) Upgrade(w http.ResponseWriter, r *http.Request) (c *Conn, err error) {
	return upgradeInner(w, r, &u.config, nil)
}

func (u *UpgradeServer) UpgradeLocalCallback(w http.ResponseWriter, r *http.Request, cb Callback) (c *Conn, err error) {
	return upgradeInner(w, r, &u.config, cb)
}

func Upgrade(w http.ResponseWriter, r *http.Request, opts ...ServerOption) (c *Conn, err error) {
	var conf ConnOption
	conf.defaultSetting()
	for _, o := range opts {
		o(&conf)
	}

	conf.defaultSettingAfter()
	return upgradeInner(w, r, &conf.Config, nil)
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

func upgradeInner(w http.ResponseWriter, r *http.Request, conf *Config, cb Callback) (wsCon *Conn, err error) {
	if conf.multiEventLoop == nil {
		return nil, ErrEventLoopEmpty
	}

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
	if !conf.disableBufioClearHack {
		// bufio2.ClearReadWriter(rw)
	}

	// 是否打开解压缩
	// 外层接收压缩, 并且客户端发送扩展过来
	var pd deflate.PermessageDeflateConf
	if conf.Decompression {
		pd, err = deflate.GetConnPermessageDeflate(r.Header)
		if err != nil {
			return nil, err
		}
	}

	buf := bytespool.GetUpgradeRespBytes()

	tmpWriter := bytes.NewBuffer((*buf)[:0])
	defer func() {
		bytespool.PutUpgradeRespBytes(buf)
		tmpWriter = nil
	}()
	resetPermessageDeflate(&pd, conf)
	if err = prepareWriteResponse(r, tmpWriter, conf, pd); err != nil {
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

	if wsCon, err = newConn(int64(fd), false, conf); err != nil {
		return nil, err
	}
	wsCon.pd = pd
	wsCon.Callback = cb
	if cb == nil {
		wsCon.Callback = conf.cb
	}
	wsCon.Callback.OnOpen(wsCon)
	if wsCon.Callback == nil {
		panic("callback is nil")
	}
	if err = conf.multiEventLoop.add(wsCon); err != nil {
		return nil, err
	}

	return wsCon, nil
}

func resetPermessageDeflate(pd *deflate.PermessageDeflateConf, conf *Config) {
	pd.Decompression = pd.Enable && conf.Decompression
	pd.Compression = pd.Enable && conf.Compression
	pd.ServerContextTakeover = pd.Enable && pd.ServerContextTakeover && conf.ServerContextTakeover
	pd.ClientContextTakeover = pd.Enable && pd.ClientContextTakeover && conf.ClientContextTakeover
}
