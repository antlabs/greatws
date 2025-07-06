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
	// err := c.WriteMessage(greatws.Binary, make([]byte, 1<<28))
	// if err != nil {
	// 	fmt.Printf("%s\n", err)
	// }
	// fmt.Printf("OnOpen: %p\n", c)
}

func (e *echoHandler) OnMessage(c *greatws.Conn, op greatws.Opcode, msg []byte) {
	// fmt.Printf("OnMessage: %s, len(%d), op:%d\n", msg, len(msg), op)
	// if err := c.WriteTimeout(op, msg, 3*time.Second); err != nil {
	// 	fmt.Println("write fail:", err)
	// }
	if err := c.WriteMessage(op, msg); err != nil {
		slog.Error("write fail:", "err", err.Error())
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

func (h *handler) echoRunOneByOne(w http.ResponseWriter, r *http.Request) {
	opts := []greatws.ServerOption{
		greatws.WithServerReplyPing(),
		greatws.WithServerDecompression(),
		greatws.WithServerIgnorePong(),
		greatws.WithServerCallback(&echoHandler{}),
		greatws.WithServerEnableUTF8Check(),
		// greatws.WithServerReadTimeout(5 * time.Second),
		greatws.WithServerMultiEventLoop(h.m),
		greatws.WithServerOneByOneMode(),
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

func (h *handler) echoRunElastic(w http.ResponseWriter, r *http.Request) {
	opts := []greatws.ServerOption{
		greatws.WithServerReplyPing(),
		greatws.WithServerDecompression(),
		greatws.WithServerIgnorePong(),
		greatws.WithServerCallback(&echoHandler{}),
		greatws.WithServerEnableUTF8Check(),
		greatws.WithServerReadTimeout(5 * time.Second),
		greatws.WithServerMultiEventLoop(h.m),
		greatws.WithServerElasticMode(),
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

// 1.测试不接管上下文，只解压
func (h *handler) echoNoContextDecompression(w http.ResponseWriter, r *http.Request) {
	c, err := greatws.Upgrade(w, r,
		greatws.WithServerReplyPing(),
		greatws.WithServerDecompression(),
		greatws.WithServerIgnorePong(),
		greatws.WithServerCallback(&echoHandler{}),
		greatws.WithServerEnableUTF8Check(),
		greatws.WithServerMultiEventLoop(h.m),
	)
	if err != nil {
		fmt.Println("Upgrade fail:", err)
		return
	}

	_ = c.ReadLoop()
}

// 2.测试不接管上下文，压缩和解压
func (h *handler) echoNoContextDecompressionAndCompression(w http.ResponseWriter, r *http.Request) {
	c, err := greatws.Upgrade(w, r,
		greatws.WithServerReplyPing(),
		greatws.WithServerDecompressAndCompress(),
		greatws.WithServerIgnorePong(),
		greatws.WithServerCallback(&echoHandler{}),
		greatws.WithServerEnableUTF8Check(),
		greatws.WithServerMultiEventLoop(h.m),
	)
	if err != nil {
		fmt.Println("Upgrade fail:", err)
		return
	}

	_ = c.ReadLoop()
}

// 3.测试接管上下文，解压
func (h *handler) echoContextTakeoverDecompression(w http.ResponseWriter, r *http.Request) {
	c, err := greatws.Upgrade(w, r,
		greatws.WithServerReplyPing(),
		greatws.WithServerDecompression(),
		greatws.WithServerIgnorePong(),
		greatws.WithServerContextTakeover(),
		greatws.WithServerCallback(&echoHandler{}),
		greatws.WithServerEnableUTF8Check(),
		greatws.WithServerMultiEventLoop(h.m),
	)
	if err != nil {
		fmt.Println("Upgrade fail:", err)
		return
	}

	_ = c.ReadLoop()
}

// 4.测试接管上下文，压缩/解压缩
func (h *handler) echoContextTakeoverDecompressionAndCompression(w http.ResponseWriter, r *http.Request) {
	c, err := greatws.Upgrade(w, r,
		greatws.WithServerReplyPing(),
		greatws.WithServerDecompressAndCompress(),
		greatws.WithServerIgnorePong(),
		greatws.WithServerContextTakeover(),
		greatws.WithServerCallback(&echoHandler{}),
		greatws.WithServerEnableUTF8Check(),
		greatws.WithServerMultiEventLoop(h.m),
	)
	if err != nil {
		fmt.Println("Upgrade fail:", err)
		return
	}

	_ = c.ReadLoop()
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
		greatws.WithMaxEventNum(256),
		greatws.WithLogLevel(slog.LevelError)) // epoll, kqueue
	h.m.Start()

	parseLoopOpt := []greatws.EvOption{
		greatws.WithBusinessGoNum(50, 10, 10000),
		greatws.WithMaxEventNum(1000),
		greatws.WithLogLevel(slog.LevelError),
	}

	h.parseLoop = greatws.NewMultiEventLoopMust(parseLoopOpt...) // epoll, kqueue
	h.parseLoop.Start()

	fmt.Printf("apiname:%s\n", h.m.GetApiName())

	go func() {
		for {
			time.Sleep(time.Second)
			fmt.Printf("curConn:%d, curTask:%d\n", h.m.GetCurConnNum(), h.m.GetCurTaskNum())
		}
	}()
	mux := &http.ServeMux{}
	mux.HandleFunc("/autobahn-io", h.echoRunInIo)
	mux.HandleFunc("/autobahn-onebyone", h.echoRunOneByOne)
	mux.HandleFunc("/autobahn-elastic", h.echoRunElastic)
	mux.HandleFunc("/no-context-takeover-decompression", h.echoNoContextDecompression)
	mux.HandleFunc("/no-context-takeover-decompression-and-compression", h.echoNoContextDecompressionAndCompression)
	mux.HandleFunc("/context-takeover-decompression", h.echoContextTakeoverDecompression)
	mux.HandleFunc("/context-takeover-decompression-and-compression", h.echoContextTakeoverDecompressionAndCompression)

	rawTCP, err := net.Listen("tcp", ":9004")
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

	lnTLS, err := tls.Listen("tcp", "localhost:9005", tlsConfig)
	if err != nil {
		panic(err)
	}

	log.Println("tls server exit:", http.Serve(lnTLS, mux))
}
