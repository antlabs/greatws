package main

import (
	"crypto/tls"
	_ "embed"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"time"

	"github.com/antlabs/greatws"
)

//go:embed public.crt
var certPEMBlock []byte

//go:embed privatekey.pem
var keyPEMBlock []byte

type echoHandler struct{}

func (e *echoHandler) OnOpen(c *greatws.Conn) {
	// fmt.Printf("OnOpen: %p\n", c)
}

func (e *echoHandler) OnMessage(c *greatws.Conn, op greatws.Opcode, msg []byte) {
	// fmt.Printf("OnMessage: %s, len(%d), op:%d\n", msg, len(msg), op)
	// if err := c.WriteTimeout(op, msg, 3*time.Second); err != nil {
	// 	fmt.Println("write fail:", err)
	// }
	if err := c.WriteMessage(op, msg); err != nil {
		slog.Error("write fail:", err)
	}
}

func (e *echoHandler) OnClose(c *greatws.Conn, err error) {
	defer c.Close()
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	slog.Error("OnClose:", "err", errMsg)
}

type handler struct {
	m *greatws.MultiEventLoop
}

func (h *handler) echo(w http.ResponseWriter, r *http.Request) {
	c, err := greatws.Upgrade(w, r,
		greatws.WithServerReplyPing(),
		greatws.WithServerDecompression(),
		greatws.WithServerIgnorePong(),
		greatws.WithServerCallback(&echoHandler{}),
		greatws.WithServerEnableUTF8Check(),
		greatws.WithServerReadTimeout(5*time.Second),
		greatws.WithServerMultiEventLoop(h.m),
	)
	if err != nil {
		slog.Error("Upgrade fail:", "err", err.Error())
	}
	_ = c
}

func main() {
	var h handler

	// debug io-uring
	// h.m = greatws.NewMultiEventLoopMust(greatws.WithEventLoops(0), greatws.WithMaxEventNum(1000), greatws.WithIoUring(), greatws.WithLogLevel(slog.LevelDebug))
	h.m = greatws.NewMultiEventLoopMust(
		greatws.WithEventLoops(runtime.NumCPU()/2),
		greatws.WithBusinessGoNum(50, 10, 10000),
		greatws.WithMaxEventNum(1000),
		greatws.WithLogLevel(slog.LevelError)) // epoll, kqueue
	h.m.Start()
	fmt.Printf("apiname:%s\n", h.m.GetApiName())

	go func() {
		for {
			time.Sleep(time.Second)
			fmt.Printf("curConn:%d, curTask:%d\n", h.m.GetCurConnNum(), h.m.GetCurTaskNum())
		}
	}()
	mux := &http.ServeMux{}
	mux.HandleFunc("/autobahn", h.echo)

	rawTCP, err := net.Listen("tcp", ":9001")
	if err != nil {
		fmt.Println("Listen fail:", err)
		return
	}

	go func() {
		log.Println("non-tls server exit:", http.Serve(rawTCP, mux))
	}()

	cert, err := tls.X509KeyPair(certPEMBlock, keyPEMBlock)
	if err != nil {
		log.Fatalf("tls.X509KeyPair failed: %v", err)
	}
	tlsConfig := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true,
	}

	lnTLS, err := tls.Listen("tcp", "localhost:9002", tlsConfig)
	if err != nil {
		panic(err)
	}

	log.Println("tls server exit:", http.Serve(lnTLS, mux))
}
