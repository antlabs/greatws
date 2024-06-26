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
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/antlabs/wsutil/bytespool"
	"github.com/antlabs/wsutil/deflate"
	"github.com/antlabs/wsutil/enum"
	"github.com/antlabs/wsutil/hostname"
)

var (
	defaultTimeout = time.Minute * 30
)

type DialOption struct {
	Header               http.Header
	u                    *url.URL
	tlsConfig            *tls.Config
	dialTimeout          time.Duration
	bindClientHttpHeader *http.Header // 握手成功之后, 客户端获取http.Header,
	Config
}

func ClientOptionToConf(opts ...ClientOption) *DialOption {
	var dial DialOption
	dial.defaultSetting()
	for _, o := range opts {
		o(&dial)
	}
	dial.defaultSettingAfter()
	return &dial
}

func DialConf(rawUrl string, conf *DialOption) (*Conn, error) {
	u, err := url.Parse(rawUrl)
	if err != nil {
		return nil, err
	}

	conf.u = u
	conf.dialTimeout = defaultTimeout
	if conf.Header == nil {
		conf.Header = make(http.Header)
	}

	return conf.Dial()
}

// https://datatracker.ietf.org/doc/html/rfc6455#section-4.1
// 又是一顿if else, 咬文嚼字
func Dial(rawUrl string, opts ...ClientOption) (*Conn, error) {
	var dial DialOption
	u, err := url.Parse(rawUrl)
	if err != nil {
		return nil, err
	}

	dial.u = u
	dial.dialTimeout = defaultTimeout
	if dial.Header == nil {
		dial.Header = make(http.Header)
	}

	dial.defaultSetting()
	for _, o := range opts {
		o(&dial)
	}

	dial.defaultSettingAfter()
	return dial.Dial()
}

// 准备握手的数据
func (d *DialOption) handshake() (*http.Request, string, error) {
	switch {
	case d.u.Scheme == "wss":
		d.u.Scheme = "https"
	case d.u.Scheme == "ws":
		d.u.Scheme = "http"
	default:
		return nil, "", fmt.Errorf("Unknown scheme, only supports ws:// or wss://: got %s", d.u.Scheme)
	}

	// 满足4.1
	// 第2点 GET约束http 1.1版本约束
	req, err := http.NewRequest("GET", d.u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	// 第5点
	d.Header.Add("Upgrade", "websocket")
	// 第6点
	d.Header.Add("Connection", "Upgrade")
	// 第7点
	secWebSocket := secWebSocketAccept()
	d.Header.Add("Sec-WebSocket-Key", secWebSocket)
	// TODO 第8点
	// 第9点
	d.Header.Add("Sec-WebSocket-Version", "13")

	if d.Decompression && d.Compression {
		d.Header.Add("Sec-WebSocket-Extensions", deflate.GenSecWebSocketExtensions(d.PermessageDeflateConf))
	}

	req.Header = d.Header
	return req, secWebSocket, nil
}

// 检查服务端响应的数据
// 4.2.2.5
func (d *DialOption) validateRsp(rsp *http.Response, secWebSocket string) error {
	if rsp.StatusCode != 101 {
		return fmt.Errorf("%w %d", ErrWrongStatusCode, rsp.StatusCode)
	}

	// 第2点
	if !strings.EqualFold(rsp.Header.Get("Upgrade"), "websocket") {
		return ErrUpgradeFieldValue
	}

	// 第3点
	if !strings.EqualFold(rsp.Header.Get("Connection"), "Upgrade") {
		return ErrConnectionFieldValue
	}

	// 第4点
	if !strings.EqualFold(rsp.Header.Get("Sec-WebSocket-Accept"), secWebSocketAcceptVal(secWebSocket)) {
		return ErrSecWebSocketAccept
	}

	// TODO 5点

	// TODO 6点
	return nil
}

// wss已经修改为https
func (d *DialOption) tlsConn(c net.Conn) net.Conn {
	if d.u.Scheme == "https" {
		cfg := d.tlsConfig
		if cfg == nil {
			cfg = &tls.Config{}
		} else {
			cfg = cfg.Clone()
		}

		if cfg.ServerName == "" {
			host := d.u.Host
			if pos := strings.Index(host, ":"); pos != -1 {
				host = host[:pos]
			}
			cfg.ServerName = host
		}
		return tls.Client(c, cfg)
	}

	return c
}

func (d *DialOption) Dial() (wsCon *Conn, err error) {
	if d.Config.multiEventLoop == nil {
		return nil, ErrEventLoopEmpty
	}

	if !d.Config.multiEventLoop.isStart() {
		return nil, ErrEventLoopNotStart
	}
	req, secWebSocket, err := d.handshake()
	if err != nil {
		return nil, err
	}

	hostName := hostname.GetHostName(d.u)
	var conn net.Conn
	conn, err = net.DialTimeout("tcp", hostName, d.dialTimeout)
	if err != nil {
		return nil, fmt.Errorf("net.Dial:%w", err)
	}

	err = conn.SetDeadline(time.Time{})
	conn = d.tlsConn(conn)
	defer func() {
		if err != nil && conn != nil {
			conn.Close()
			conn = nil
		}
	}()

	if err = req.Write(conn); err != nil {
		return nil, fmt.Errorf("write req fail:%w", err)
	}

	br := bufio.NewReader(bufio.NewReader(conn))
	rsp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, err
	}

	if d.bindClientHttpHeader != nil {
		*d.bindClientHttpHeader = rsp.Header.Clone()
	}

	pd, err := deflate.GetConnPermessageDeflate(rsp.Header)
	if err != nil {
		return nil, err
	}
	if d.Decompression {
		pd.Decompression = pd.Enable && d.Decompression
	}
	if d.Compression {
		pd.Compression = pd.Enable && d.Compression
	}

	if err = d.validateRsp(rsp, secWebSocket); err != nil {
		return
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
	if wsCon, err = newConn(int64(fd), true, &d.Config); err != nil {
		return nil, err
	}
	wsCon.pd = pd
	wsCon.Callback = d.cb
	wsCon.OnOpen(wsCon)
	if br.Buffered() > 0 {
		b, err := br.Peek(br.Buffered())
		if err != nil {
			return nil, err
		}

		wsCon.rbuf = bytespool.GetBytes(len(b) + enum.MaxFrameHeaderSize)

		copy(*wsCon.rbuf, b)
		wsCon.rw = len(b)
		if err = wsCon.processHeaderPayloadCallback(); err != nil {
			return nil, err
		}
	}
	if err = d.Config.multiEventLoop.add(wsCon); err != nil {
		return nil, err
	}
	return wsCon, nil
}
