// Copyright 2021-2024 antlabs. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package websocket

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 测试客户端Dial, 返回的http.Header
func Test_Client_Dial_Check_Header(t *testing.T) {

	t.Run("Dial: valid resp: status code fail", func(t *testing.T) {
		done := make(chan bool, 1)
		run := int32(0)
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&run, int32(1))
			done <- true
		}))

		defer ts.Close()

		rawURL := strings.ReplaceAll(ts.URL, "http", "ws")
		_, err := Dial(rawURL)
		if err == nil {
			t.Fatal("should be error")
		}
	})

	t.Run("DialConf: valid resp : status code fail", func(t *testing.T) {
		done := make(chan bool, 1)
		run := int32(0)
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&run, int32(1))
			done <- true
		}))

		defer ts.Close()

		cnf := ClientOptionToConf()
		rawURL := strings.ReplaceAll(ts.URL, "http", "ws")
		_, err := DialConf(rawURL, cnf)
		if err == nil {
			t.Fatal("should be error")
		}
	})

	t.Run("Dial: valid resp: Upgrade field fail", func(t *testing.T) {
		done := make(chan bool, 1)
		run := int32(0)
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&run, int32(1))
			w.WriteHeader(101)
			w.Header().Set("Upgrade", "xx")
			done <- true
		}))

		defer ts.Close()

		rawURL := strings.ReplaceAll(ts.URL, "http", "ws")
		_, err := Dial(rawURL)
		if err == nil {
			t.Fatal("should be error")
		}
	})

	t.Run("DialConf: valid resp: Upgrade field fail", func(t *testing.T) {
		done := make(chan bool, 1)
		run := int32(0)
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&run, int32(1))
			w.WriteHeader(101)
			w.Header().Set("Upgrade", "xx")
			done <- true
		}))

		defer ts.Close()

		cnf := ClientOptionToConf()
		rawURL := strings.ReplaceAll(ts.URL, "http", "ws")
		_, err := DialConf(rawURL, cnf)
		if err == nil {
			t.Fatal("should be error")
		}
	})

	t.Run("Dial: valid resp: Connection fail", func(t *testing.T) {
		done := make(chan bool, 1)
		run := int32(0)
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&run, int32(1))
			w.Header().Set("Upgrade", "websocket")
			w.Header().Set("Connection", "xx")
			w.WriteHeader(101)
			done <- true
		}))

		defer ts.Close()

		rawURL := strings.ReplaceAll(ts.URL, "http", "ws")
		_, err := Dial(rawURL)
		if err == nil {
			t.Fatal("should be error")
		}
	})

	t.Run("DialConf: valid resp: Connection fail", func(t *testing.T) {
		done := make(chan bool, 1)
		run := int32(0)
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&run, int32(1))
			w.Header().Set("Upgrade", "websocket")
			w.Header().Set("Connection", "xx")
			w.WriteHeader(101)
			done <- true
		}))

		defer ts.Close()

		cnf := ClientOptionToConf()
		rawURL := strings.ReplaceAll(ts.URL, "http", "ws")
		_, err := DialConf(rawURL, cnf)
		if err == nil {
			t.Fatal("should be error")
		} else {
			fmt.Printf("err: %v\n", err)
		}
	})

	t.Run("Dial: valid resp: Sec-WebSocket-Accept fail", func(t *testing.T) {
		done := make(chan bool, 1)
		run := int32(0)
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&run, int32(1))
			w.Header().Set("Upgrade", "websocket")
			w.Header().Set("Connection", "Upgrade")
			w.WriteHeader(101)
			done <- true
		}))

		defer ts.Close()

		rawURL := strings.ReplaceAll(ts.URL, "http", "ws")
		_, err := Dial(rawURL)
		if err == nil {
			t.Fatal("should be error")
		}
	})

	t.Run("DialConf: valid resp: Sec-WebSocket-Accept fail", func(t *testing.T) {
		done := make(chan bool, 1)
		run := int32(0)
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&run, int32(1))
			w.Header().Set("Upgrade", "websocket")
			w.Header().Set("Connection", "Upgrade")
			w.WriteHeader(101)
			done <- true
		}))

		defer ts.Close()

		cnf := ClientOptionToConf()
		rawURL := strings.ReplaceAll(ts.URL, "http", "ws")
		_, err := DialConf(rawURL, cnf)
		if err == nil {
			t.Fatal("should be error")
		} else {
			fmt.Printf("err: %v\n", err)
		}
	})
}

func Test_Client_Dial_HandshakeTimeout(t *testing.T) {
	m := NewMultiEventLoopAndStartMust(WithEventLoops(1), WithBusinessGoNum(1, 1, 1))
	for _, dialConf := range []bool{false, true} {
		for _, useTLS := range []bool{false, true} {
			t.Run(fmt.Sprintf("DialConf=%t/TLS=%t", dialConf, useTLS), func(t *testing.T) {
				started := make(chan struct{})
				release := make(chan struct{})
				ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					close(started)
					<-release
				}))
				if useTLS {
					ts.StartTLS()
				} else {
					ts.Start()
				}
				defer ts.Close()
				defer close(release)
				opts := []ClientOption{WithClientDialTimeout(200 * time.Millisecond), WithClientMultiEventLoop(m)}
				if useTLS {
					opts = append(opts, WithClientTLSConfig(&tls.Config{InsecureSkipVerify: true}))
				}
				result := make(chan error, 1)
				go func() {
					var err error
					if dialConf {
						_, err = DialConf(strings.Replace(ts.URL, "http", "ws", 1), ClientOptionToConf(opts...))
					} else {
						_, err = Dial(strings.Replace(ts.URL, "http", "ws", 1), opts...)
					}
					result <- err
				}()
				select {
				case <-started:
				case err := <-result:
					t.Fatalf("handshake did not reach the server: %v", err)
				case <-time.After(3 * time.Second):
					t.Fatal("server did not receive the handshake")
				}
				select {
				case err := <-result:
					var timeout net.Error
					if !errors.As(err, &timeout) || !timeout.Timeout() {
						t.Fatalf("expected handshake timeout, got %v", err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("handshake ignored the configured timeout")
				}
			})
		}
	}
}

func Test_Client_DialConf_TCPTimeout(t *testing.T) {
	m := NewMultiEventLoopAndStartMust(WithEventLoops(1), WithBusinessGoNum(1, 1, 1))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()
	for _, timeout := range []time.Duration{time.Nanosecond, -time.Second} {
		t.Run(timeout.String(), func(t *testing.T) {
			_, err := DialConf(strings.Replace(ts.URL, "http", "ws", 1), ClientOptionToConf(
				WithClientDialTimeout(timeout), WithClientMultiEventLoop(m),
			))
			var timeoutError net.Error
			if !errors.As(err, &timeoutError) || !timeoutError.Timeout() {
				t.Fatalf("expected TCP timeout, got %v", err)
			}
		})
	}
}

func Test_ClientOption_DialTimeout(t *testing.T) {
	for _, tt := range []struct {
		name    string
		opts    []ClientOption
		timeout time.Duration
	}{
		{name: "default", timeout: defaultTimeout},
		{name: "custom", opts: []ClientOption{WithClientDialTimeout(time.Second)}, timeout: time.Second},
		{name: "disabled", opts: []ClientOption{WithClientDialTimeout(0)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conf := ClientOptionToConf(tt.opts...)
			if conf.dialTimeout != tt.timeout {
				t.Fatalf("timeout = %v, want %v", conf.dialTimeout, tt.timeout)
			}
		})
	}
}

func Test_Client_Dial_TLSHandshakeTimeout(t *testing.T) {
	m := NewMultiEventLoopAndStartMust(WithEventLoops(1), WithBusinessGoNum(1, 1, 1))
	for _, dialConf := range []bool{false, true} {
		t.Run(fmt.Sprintf("DialConf=%t", dialConf), func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			closed := make(chan error, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					closed <- err
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				_, err = io.Copy(io.Discard, conn)
				closed <- err
			}()
			opts := []ClientOption{WithClientDialTimeout(200 * time.Millisecond), WithClientMultiEventLoop(m)}
			result := make(chan error, 1)
			go func() {
				var err error
				if dialConf {
					_, err = DialConf("wss://"+ln.Addr().String(), ClientOptionToConf(opts...))
				} else {
					_, err = Dial("wss://"+ln.Addr().String(), opts...)
				}
				result <- err
			}()
			select {
			case err := <-result:
				var timeout net.Error
				if !errors.As(err, &timeout) || !timeout.Timeout() {
					t.Fatalf("expected TLS handshake timeout, got %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("TLS handshake ignored the configured timeout")
			}
			select {
			case err := <-closed:
				if err != nil {
					t.Fatalf("timed out connection was not closed: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out connection was not closed")
			}
		})
	}
}

func Test_Client_Dial_TimeoutAfterUpgrade(t *testing.T) {
	m := NewMultiEventLoopAndStartMust(WithEventLoops(1), WithBusinessGoNum(1, 1, 1))
	for _, dialConf := range []bool{false, true} {
		for _, timeout := range []time.Duration{0, 200 * time.Millisecond} {
			t.Run(fmt.Sprintf("DialConf=%t/timeout=%s", dialConf, timeout), func(t *testing.T) {
				release := make(chan struct{})
				handlerStarted := make(chan struct{})
				handlerDone := make(chan struct{})
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					close(handlerStarted)
					defer close(handlerDone)
					if timeout == 0 {
						time.Sleep(400 * time.Millisecond)
					}
					conn, err := Upgrade(w, r, WithServerMultiEventLoop(m))
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					time.Sleep(400 * time.Millisecond)
					if err := conn.WriteMessage(Text, []byte("after timeout")); err != nil {
						t.Error(err)
					}
					<-release
				}))
				defer func() {
					close(release)
					ts.Close()
					select {
					case <-handlerStarted:
						<-handlerDone
					default:
					}
				}()
				messages := make(chan string, 1)
				opts := []ClientOption{
					WithClientDialTimeout(timeout), WithClientMultiEventLoop(m),
					WithClientOnMessageFunc(func(c *Conn, op Opcode, data []byte) { messages <- string(data) }),
				}
				var conn *Conn
				var err error
				if dialConf {
					conn, err = DialConf(strings.Replace(ts.URL, "http", "ws", 1), ClientOptionToConf(opts...))
				} else {
					conn, err = Dial(strings.Replace(ts.URL, "http", "ws", 1), opts...)
				}
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				select {
				case message := <-messages:
					if message != "after timeout" {
						t.Fatalf("unexpected message: %q", message)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("connection stopped receiving after the handshake timeout")
				}
			})
		}
	}
}
