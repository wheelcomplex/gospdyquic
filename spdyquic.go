package gospdyquic

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"time"

	"github.com/bradfitz/http2"
	"github.com/devsisters/goquic"
	"github.com/devsisters/gospdyquic/spdy"
	"github.com/vanillahsu/go_reuseport"
)

type SpdyStream struct {
	closed          bool
	stream_id       uint32
	header          http.Header
	header_parsed   bool
	buffer          *bytes.Buffer
	server          *QuicSpdyServer
	sessionFnChan   chan func()
	closeNotifyChan chan bool
}

type spdyResponseWriter struct {
	serverStream  goquic.QuicStream
	spdyStream    *SpdyStream
	header        http.Header
	wroteHeader   bool
	sessionFnChan chan func()
}

func (w *spdyResponseWriter) Header() http.Header {
	return w.header
}

func (w *spdyResponseWriter) Write(buffer []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}

	copiedBuffer := make([]byte, len(buffer))
	copy(copiedBuffer, buffer)
	w.sessionFnChan <- func() {
		if w.spdyStream.closed {
			return
		}
		w.serverStream.WriteOrBufferData(copiedBuffer, false)
	}
	return len(buffer), nil
}

func cloneHeader(h http.Header) http.Header {
	h2 := make(http.Header, len(h))
	for k, vv := range h {
		vv2 := make([]string, len(vv))
		copy(vv2, vv)
		h2[k] = vv2
	}
	return h2
}

func (w *spdyResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	copiedHeader := cloneHeader(w.header)
	w.sessionFnChan <- func() {
		copiedHeader.Set(":status", strconv.Itoa(statusCode))
		if w.spdyStream.closed {
			return
		}
		w.serverStream.WriteHeader(copiedHeader, false)
	}
	w.wroteHeader = true
}

func (w *spdyResponseWriter) CloseNotify() <-chan bool {
	return w.spdyStream.closeNotify()
}

func (w *spdyResponseWriter) Flush() {
	// TODO(serialx): Support flush
	// Maybe it's not neccessary because QUIC sends packets in a paced interval.
	// I cannot find any flush related functions in current QUIC code,
	// and samples needing Flush seems to work fine.
	// This functionality maybe needed in the future when we plan to buffer user
	// writes in the Go side.
}

func (stream *SpdyStream) ProcessData(serverStream goquic.QuicStream, newBytes []byte) int {
	stream.buffer.Write(newBytes)

	if !stream.header_parsed {
		// We don't want to consume the buffer *yet*, so create a new reader
		reader := bytes.NewReader(stream.buffer.Bytes())
		header, err := spdy.ParseHeaders(reader)
		if err != nil {
			// Header parsing unsuccessful, maybe header is not yet completely received
			// Append it to the buffer for parsing later
			return int(len(newBytes))
		}

		// Header parsing successful
		n, _ := reader.Seek(0, 1)
		// Consume the buffer, the rest of the buffer is the body
		stream.buffer.Next(int(n))

		stream.header_parsed = true
		stream.header = header

		// TODO(serialx): Parsing header should also exist on OnFinRead
	}
	// Process body
	return len(newBytes)
}

func (stream *SpdyStream) OnFinRead(quicStream goquic.QuicStream) {
	if !stream.header_parsed {
		// TODO(serialx): Send error message
	}
	quicStream.CloseReadSide()

	header := stream.header
	req := new(http.Request)
	req.Method = header.Get(":method")
	req.RequestURI = header.Get(":path")
	req.Proto = header.Get(":version")
	req.Header = header
	req.Host = header.Get(":host")
	// req.RemoteAddr = serverStream. TODO(serialx): Add remote addr
	rawPath := header.Get(":path")

	url, err := url.ParseRequestURI(rawPath)
	if err != nil {
		return
		// TODO(serialx): Send error message
	}

	url.Scheme = header.Get(":scheme")
	url.Host = header.Get(":host")
	req.URL = url
	// TODO(serialx): To buffered async read
	req.Body = ioutil.NopCloser(stream.buffer)

	go func() {
		w := &spdyResponseWriter{
			serverStream:  quicStream,
			spdyStream:    stream,
			header:        make(http.Header),
			sessionFnChan: stream.sessionFnChan,
		}
		if stream.server.Handler != nil {
			stream.server.Handler.ServeHTTP(w, req)
		} else {
			http.DefaultServeMux.ServeHTTP(w, req)
		}

		stream.sessionFnChan <- func() {
			if stream.closed {
				return
			}
			quicStream.WriteOrBufferData(make([]byte, 0), true)
		}
	}()
}

func (stream *SpdyStream) OnClose(quicStream goquic.QuicStream) {
	if stream.closeNotifyChan != nil && !stream.closed {
		stream.closeNotifyChan <- true
	}
	stream.closed = true
}

func (stream *SpdyStream) closeNotify() <-chan bool {
	if stream.closeNotifyChan == nil {
		stream.closeNotifyChan = make(chan bool, 1)
	}
	return stream.closeNotifyChan
}

type SpdySession struct {
	server        *QuicSpdyServer
	sessionFnChan chan func()
}

func (s *SpdySession) CreateIncomingDataStream(stream_id uint32) goquic.DataStreamProcessor {
	stream := &SpdyStream{
		stream_id:     stream_id,
		header_parsed: false,
		server:        s.server,
		buffer:        new(bytes.Buffer),
		sessionFnChan: s.sessionFnChan,
	}
	return stream
}

func (s *SpdySession) CreateOutgoingDataStream() goquic.DataStreamProcessor {
	// NOT SUPPORTED
	return nil
}

type QuicSpdyServer struct {
	Addr           string
	Handler        http.Handler
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration
	MaxHeaderBytes int
	numOfServers   int
	Certificate    tls.Certificate
	isSecure       bool
	sessionFnChan  chan func()
}

func (srv *QuicSpdyServer) ListenAndServe() error {
	addr := srv.Addr
	if addr == "" {
		addr = ":http"
	}

	readChanArray := make([](chan goquic.UdpData), srv.numOfServers)
	writerArray := make([](*goquic.ServerWriter), srv.numOfServers)
	connArray := make([](*net.UDPConn), srv.numOfServers)

	// N consumers
	for i := 0; i < srv.numOfServers; i++ {
		rch := make(chan goquic.UdpData, 500)
		wch := make(chan goquic.UdpData, 500) // TODO(serialx, hodduc): Optimize buffer size

		conn, err := reuseport.NewReusablePortPacketConn("udp4", addr)
		if err != nil {
			return err
		}
		defer conn.Close()

		udp_conn, ok := conn.(*net.UDPConn)
		if !ok {
			return errors.New("ListenPacket did not return net.UDPConn")
		}
		connArray[i] = udp_conn

		listen_addr, err := net.ResolveUDPAddr("udp", udp_conn.LocalAddr().String())
		if err != nil {
			return err
		}

		readChanArray[i] = rch
		writerArray[i] = goquic.NewServerWriter(wch)
		go srv.Serve(listen_addr, writerArray[i], readChanArray[i])
	}

	// N producers
	readFunc := func(conn *net.UDPConn) {
		buf := make([]byte, 65535)

		for {
			n, peer_addr, err := conn.ReadFromUDP(buf)
			if err != nil {
				// TODO(serialx): Don't panic and keep calm...
				panic(err)
			}

			var connId uint64

			switch buf[0] & 0xC {
			case 0xC:
				connId = binary.LittleEndian.Uint64(buf[1:9])
			case 0x8:
				connId = binary.LittleEndian.Uint64(buf[1:5])
			case 0x4:
				connId = binary.LittleEndian.Uint64(buf[1:2])
			default:
				connId = 0
			}

			buf_new := make([]byte, n)
			copy(buf_new, buf[:n])

			readChanArray[connId%uint64(srv.numOfServers)] <- goquic.UdpData{Addr: peer_addr, Buf: buf_new}
			// TODO(hodduc): Minimize heap uses of buf. Consider using sync.Pool standard library to implement buffer pool.
		}
	}

	// N consumers
	writeFunc := func(conn *net.UDPConn, writer *goquic.ServerWriter) {
		for dat := range writer.Ch {
			conn.WriteToUDP(dat.Buf, dat.Addr)
		}
	}

	for i := 0; i < srv.numOfServers-1; i++ {
		go writeFunc(connArray[i], writerArray[i])
		go readFunc(connArray[i])
	}

	go writeFunc(connArray[srv.numOfServers-1], writerArray[srv.numOfServers-1])
	readFunc(connArray[srv.numOfServers-1])
	return nil
}

type ProofSource struct {
	server *QuicSpdyServer
}

func (ps *ProofSource) GetProof(addr net.IP, hostname []byte, serverConfig []byte, ecdsaOk bool) (outCerts [][]byte, outSignature []byte) {
	outCerts = make([][]byte, 0, 10)
	for _, cert := range ps.server.Certificate.Certificate {
		x509cert, err := x509.ParseCertificate(cert)
		if err != nil {
			panic(err)
		}
		outCerts = append(outCerts, x509cert.Raw)
	}
	var err error = nil

	// Generate "proof of authenticity" (See "Quic Crypto" docs for details)
	// Length of the prefix used to calculate the signature: length of label + 0x00 byte
	const kPrefixStr = "QUIC server config signature"
	const kPrefixLen = len(kPrefixStr) + 1
	//bufferToSign := make([]byte, 0, len(serverConfig)+kPrefixLen)
	bufferToSign := bytes.NewBuffer(make([]byte, 0, len(serverConfig)+kPrefixLen))
	bufferToSign.Write([]byte(kPrefixStr))
	bufferToSign.Write([]byte("\x00"))
	bufferToSign.Write(serverConfig)

	hasher := crypto.SHA256.New()
	_, err = hasher.Write(bufferToSign.Bytes())
	if err != nil {
		panic("Error while hashing")
	}
	hashSum := hasher.Sum(nil)

	switch priv := ps.server.Certificate.PrivateKey.(type) {
	case *rsa.PrivateKey:
		outSignature, err = priv.Sign(rand.Reader, hashSum, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA256})
		if err != nil {
			panic(err)
		}
	case *ecdsa.PrivateKey:
		// XXX(serialx): Not tested. Should input be a hashSum or the original message?
		//               Since there is no secure QUIC server reference implementation,
		//               only a real test with the Chrome browser would verify the code.
		//               Since I don't currently have a ECDSA certificate, no testing is done.
		outSignature, err = priv.Sign(rand.Reader, hashSum, nil)
		if err != nil {
			panic(err)
		}
	default:
		panic("Unknown form of private key")
	}
	if err != nil {
		panic(err)
	}
	return outCerts, outSignature
}

func (srv *QuicSpdyServer) Serve(listen_addr *net.UDPAddr, writer *goquic.ServerWriter, readChan chan goquic.UdpData) error {
	runtime.LockOSThread()

	sessionFnChan := make(chan func())
	proofSource := &ProofSource{server: srv}

	createSpdySession := func() goquic.DataStreamCreator {
		return &SpdySession{server: srv, sessionFnChan: sessionFnChan}
	}

	dispatcher := goquic.CreateQuicDispatcher(writer, createSpdySession, goquic.CreateTaskRunner(), proofSource, srv.isSecure)

	for {
		select {
		case result, ok := <-readChan:
			if !ok {
				break
			}
			dispatcher.ProcessPacket(listen_addr, result.Addr, result.Buf)
		case <-dispatcher.TaskRunner.WaitTimer():
			dispatcher.TaskRunner.DoTasks()
		case fn, ok := <-sessionFnChan:
			if !ok {
				break
			}
			fn()
		}
	}
}

// Provide "Alternate-Protocol" header for QUIC
func altProtoMiddleware(next http.Handler, port int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Alternate-Protocol", fmt.Sprintf("%d:quic", port))
		next.ServeHTTP(w, r)
	})
}

func parsePort(addr string) (port int, err error) {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	port, err = net.LookupPort("udp", portStr)
	if err != nil {
		return 0, err
	}

	return port, nil
}

func ListenAndServe(addr string, numOfServers int, handler http.Handler) error {
	port, err := parsePort(addr)
	if err != nil {
		return err
	}

	if handler == nil {
		handler = http.DefaultServeMux
	}

	httpServer := &http.Server{Addr: addr, Handler: altProtoMiddleware(handler, port)}
	http2.ConfigureServer(httpServer, nil)
	go httpServer.ListenAndServe()
	server := &QuicSpdyServer{Addr: addr, Handler: handler, numOfServers: numOfServers, isSecure: false}
	return server.ListenAndServe()
}

func ListenAndServeSecure(addr string, certFile string, keyFile string, numOfServers int, handler http.Handler) error {
	port, err := parsePort(addr)
	if err != nil {
		return err
	}

	if handler == nil {
		handler = http.DefaultServeMux
	}

	go func() {
		httpServer := &http.Server{Addr: addr, Handler: altProtoMiddleware(handler, port)}
		http2.ConfigureServer(httpServer, nil)
		err := httpServer.ListenAndServeTLS(certFile, keyFile)
		if err != nil {
			panic(err)
		}
	}()
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return err
	}

	server := &QuicSpdyServer{Addr: addr, Handler: handler, numOfServers: numOfServers, Certificate: cert, isSecure: true}
	return server.ListenAndServe()
}

func ListenAndServeQuicSpdyOnly(addr string, certFile string, keyFile string, numOfServers int, handler http.Handler) error {
	server := &QuicSpdyServer{Addr: addr, Handler: handler, numOfServers: numOfServers}
	return server.ListenAndServe()
}
