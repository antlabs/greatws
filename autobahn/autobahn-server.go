package main

import (
	"crypto/tls"
	_ "embed"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof"
	"runtime"
	"time"

	"github.com/antlabs/greatws"
)

var runInEventLoop = flag.Bool("run-in-event-loop", false, "run in event loop")

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
	m         *greatws.MultiEventLoop
	parseLoop *greatws.MultiEventLoop
}

// 运行在业务线程
func (h *handler) echo(w http.ResponseWriter, r *http.Request) {
	opts := []greatws.ServerOption{
		greatws.WithServerReplyPing(),
		greatws.WithServerDecompression(),
		greatws.WithServerIgnorePong(),
		greatws.WithServerCallback(&echoHandler{}),
		greatws.WithServerEnableUTF8Check(),
		greatws.WithServerReadTimeout(5 * time.Second),
		greatws.WithServerMultiEventLoop(h.m),
	}

	if *runInEventLoop {
		opts = append(opts, greatws.WithServerCallbackInEventLoop())
	}

	c, err := greatws.Upgrade(w, r, opts...)
	if err != nil {
		slog.Error("Upgrade fail:", "err", err.Error())
	}
	_ = c
}

// 运行在io线程
func (h *handler) echoRunInIo(w http.ResponseWriter, r *http.Request) {
	opts := []greatws.ServerOption{
		greatws.WithServerReplyPing(),
		greatws.WithServerDecompression(),
		greatws.WithServerIgnorePong(),
		greatws.WithServerCallback(&echoHandler{}),
		greatws.WithServerEnableUTF8Check(),
		greatws.WithServerReadTimeout(5 * time.Second),
		greatws.WithServerMultiEventLoop(h.m),
		greatws.WithServerCallbackInEventLoop(),
	}

	if *runInEventLoop {
		opts = append(opts, greatws.WithServerCallbackInEventLoop())
	}

	c, err := greatws.Upgrade(w, r, opts...)
	if err != nil {
		slog.Error("Upgrade fail:", "err", err.Error())
	}
	_ = c
}

// 使用stream模式运行， 一个websocket一个go程
func (h *handler) echoRunStream(w http.ResponseWriter, r *http.Request) {
	opts := []greatws.ServerOption{
		greatws.WithServerReplyPing(),
		greatws.WithServerDecompression(),
		greatws.WithServerIgnorePong(),
		greatws.WithServerCallback(&echoHandler{}),
		greatws.WithServerEnableUTF8Check(),
		greatws.WithServerReadTimeout(5 * time.Second),
		greatws.WithServerMultiEventLoop(h.m),
		greatws.WithServerStreamMode(),
		greatws.WithServerCallbackInEventLoop(),
	}

	if *runInEventLoop {
		opts = append(opts, greatws.WithServerCallbackInEventLoop())
	}

	c, err := greatws.Upgrade(w, r, opts...)
	if err != nil {
		slog.Error("Upgrade fail:", "err", err.Error())
	}
	_ = c
}

// 使用parse loop模式运行， 一个websocket一个go程
func (h *handler) echoRunInParseLoop(w http.ResponseWriter, r *http.Request) {
	opts := []greatws.ServerOption{
		greatws.WithServerReplyPing(),
		greatws.WithServerDecompression(),
		greatws.WithServerIgnorePong(),
		greatws.WithServerCallback(&echoHandler{}),
		greatws.WithServerEnableUTF8Check(),
		greatws.WithServerReadTimeout(5 * time.Second),
		greatws.WithServerMultiEventLoop(h.parseLoop),
		greatws.WithServerStreamMode(),
		greatws.WithServerCallbackInEventLoop(),
	}

	if *runInEventLoop {
		opts = append(opts, greatws.WithServerCallbackInEventLoop())
	}

	c, err := greatws.Upgrade(w, r, opts...)
	if err != nil {
		slog.Error("Upgrade fail:", "err", err.Error())
	}
	_ = c
}
func main() {
	flag.Parse()

	var h handler
	runtime.SetBlockProfileRate(1)

	go func() {
		log.Println(http.ListenAndServe(":6060", nil))
	}()

	// debug io-uring
	// h.m = greatws.NewMultiEventLoopMust(greatws.WithEventLoops(0), greatws.WithMaxEventNum(1000), greatws.WithIoUring(), greatws.WithLogLevel(slog.LevelDebug))
	h.m = greatws.NewMultiEventLoopMust(
		greatws.WithEventLoops(runtime.NumCPU()/2),
		greatws.WithBusinessGoNum(50, 10, 10000),
		greatws.WithMaxEventNum(1000),
		greatws.WithLogLevel(slog.LevelError)) // epoll, kqueue
	h.m.Start()

	h.parseLoop = greatws.NewMultiEventLoopMust(
		greatws.WithEventLoops(runtime.NumCPU()/2),
		greatws.WithBusinessGoNum(50, 10, 10000),
		greatws.WithMaxEventNum(1000),
		greatws.WithParseInParseLoop(),
		greatws.WithLogLevel(slog.LevelError)) // epoll, kqueue
	h.parseLoop.Start()

	fmt.Printf("apiname:%s\n", h.m.GetApiName())

	go func() {
		for {
			time.Sleep(time.Second)
			fmt.Printf("curConn:%d, curTask:%d\n", h.m.GetCurConnNum(), h.m.GetCurTaskNum())
		}
	}()
	mux := &http.ServeMux{}
	mux.HandleFunc("/autobahn", h.echo)
	mux.HandleFunc("/autobahn-io", h.echoRunInIo)
	mux.HandleFunc("/autobahn-stream", h.echoRunStream)
	mux.HandleFunc("/autobahn-parse-loop", h.echoRunInParseLoop)

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
