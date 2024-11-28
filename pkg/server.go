package pkg

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ServerContextKey is a context key. It can be used in HTTP
	// handlers with Context.Value to access the server that
	// started the handler. The associated value will be of
	// type *Server.
	ServerContextKey = &contextKey{"http-server"}

	// LocalAddrContextKey is a context key. It can be used in
	// HTTP handlers with Context.Value to access the local
	// address the connection arrived on.
	// The associated value will be of type net.Addr.
	LocalAddrContextKey = &contextKey{"local-addr"}
)

// This is pools to make garbage collector works much easier
// because the buffer reader and writer and heavy object storing them after resetting here
var (
	bufioReaderPool   sync.Pool
	bufioWriter2kPool sync.Pool
	bufioWriter4kPool sync.Pool
)

type conn struct {
	// server is the server on which the connection arrived.
	// Immutable; never nil.
	server *MiniServer

	// cancelCtx cancels the connection-level context.
	cancelCtx context.CancelFunc

	// rwc is the underlying network connection.
	// This is never wrapped by other types and is the value given out
	// to CloseNotifier callers. It is usually of type *net.TCPConn or
	// *tls.Conn.
	rwc net.Conn

	// remoteAddr is rwc.RemoteAddr().String(). It is not populated synchronously
	// inside the Listener's Accept goroutine, as some implementations block.
	// It is populated immediately inside the (*conn).serve goroutine.
	// This is the value of a Handler's (*Request).RemoteAddr.
	remoteAddr string

	// werr is set to the first write error to rwc.
	// It is set via checkConnErrorWriter{w}, where bufw writes.
	werr error

	// bufr reads from r.
	bufr *bufio.Reader

	// bufw writes to checkConnErrorWriter{c}, which populates werr on error.
	bufw *bufio.Writer

	// lastMethod is the method of the most recent request
	// on this connection, if any.
	lastMethod string

	// r is bufr's read source. It's a wrapper around rwc that provides
	// io.LimitedReader-style limiting (while reading request headers)
	// and functionality to support CloseNotifier. See *connReader docs.
	r *connReader

	curReq atomic.Pointer[response] // (which has a Request in it)

	curState atomic.Uint64 // packed (unixtime<<8|uint8(ConnState))

}

type MiniServer struct {
	// Addr optionally specifies the TCP address for the server to listen on,
	// in the form "host:port". If empty, ":http" (port 80) is used.
	Addr string

	Handler http.Handler // Should always be present unlike the go standard library

	// DisableGeneralOptionsHandler, if true, passes "OPTIONS *" requests to the Handler,
	// otherwise responds with 200 OK and Content-Length: 0.
	DisableGeneralOptionsHandler bool

	// Certificate is a replacement of the tls config since the server does not needed to be configured
	// to the absolute
	Certificate tls.Certificate

	// SupportH2 whether the current server should support HTTP/2.0
	SupportH2 bool

	// ReadTimeout is the maximum duration for reading the entire
	// request, including the body. A zero or negative value means
	// there will be no timeout.
	//
	// Because ReadTimeout does not let Handlers make per-request
	// decisions on each request body's acceptable deadline or
	// upload rate, most users will prefer to use
	// ReadHeaderTimeout. It is valid to use them both.
	ReadTimeout time.Duration

	// ReadHeaderTimeout is the amount of time allowed to read
	// request headers. The connection's read deadline is reset
	// after reading the headers and the Handler can decide what
	// is considered too slow for the body. If ReadHeaderTimeout
	// is zero, the value of ReadTimeout is used. If both are
	// zero, there is no timeout.
	ReadHeaderTimeout time.Duration

	// WriteTimeout is the maximum duration before timing out
	// writes of the response. It is reset whenever a new
	// request's header is read. Like ReadTimeout, it does not
	// let Handlers make decisions on a per-request basis.
	// A zero or negative value means there will be no timeout.
	WriteTimeout time.Duration

	// IdleTimeout is the maximum amount of time to wait for the
	// next request when keep-alive s are enabled. If IdleTimeout
	// is zero, the value of ReadTimeout is used. If both are
	// zero, there is no timeout.
	IdleTimeout time.Duration

	// MaxHeaderBytes controls the maximum number of bytes the
	// server will read parsing the request header's keys and
	// values, including the request line. It does not limit the
	// size of the request body.
	// If zero, DefaultMaxHeaderBytes is used.
	MaxHeaderBytes int

	inShutdown atomic.Bool // true when server is in shutdown

	// ErrorLog specifies an optional logger for errors accepting
	// connections, unexpected behavior from handlers, and
	// underlying FileSystem errors.
	// If nil, logging is done via the log package's standard logger.
	ErrorLog *log.Logger

	mu         sync.Mutex
	activeConn map[*conn]struct{}
}

// chunkWriter writes to a response's conn buffer, and is the writer
// wrapped by the response.w buffered writer.
//
// chunkWriter also is responsible for finalizing the Header, including
// conditionally setting the Content-Type and setting a Content-Length
// in cases where the handler's final output is smaller than the buffer
// size. It also conditionally adds chunk headers, when in chunking mode.
//
// See the comment above (*response).Write for the entire write flow.
type chunkWriter struct {
	res *response

	// header is either nil or a deep clone of res.handlerHeader
	// at the time of res.writeHeader, if res.writeHeader is
	// called and extra buffering is being done to calculate
	// Content-Type and/or Content-Length.
	header http.Header

	// wroteHeader tells whether the header's been written to "the
	// wire" (or rather: w.conn.buf). this is unlike
	// (*response).wroteHeader, which tells only whether it was
	// logically written.
	wroteHeader bool

	// set by the writeHeader method:
	chunking bool // using chunked transfer encoding for reply body
}

type response struct {
	conn             *conn
	req              *http.Request // request for this response
	reqBody          io.ReadCloser
	cancelCtx        context.CancelFunc // when ServeHTTP exits
	wroteHeader      bool               // a non-1xx header has been (logically) written
	wroteContinue    bool               // 100 Continue response was written
	wants10KeepAlive bool               // HTTP/1.0 w/ Connection "keep-alive"
	wantsClose       bool               // HTTP request has Connection "close"

	// canWriteContinue is an atomic boolean that says whether or
	// not a 100 Continue header can be written to the
	// connection.
	// writeContinueMu must be held while writing the header.
	// These two fields together synchronize the body reader (the
	// expectContinueReader, which wants to write 100 Continue)
	// against the main writer.
	canWriteContinue atomic.Bool
	writeContinueMu  sync.Mutex

	w  *bufio.Writer // buffers output in chunks to chunkWriter
	cw chunkWriter

	// handlerHeader is the Header that Handlers get access to,
	// which may be retained and mutated even after WriteHeader.
	// handlerHeader is copied into cw.header at WriteHeader
	// time, and privately mutated thereafter.
	handlerHeader http.Header
	calledHeader  bool // handler accessed handlerHeader via Header

	written       int64 // number of bytes written in body
	contentLength int64 // explicitly-declared Content-Length; or -1
	status        int   // status code passed to WriteHeader

	// close connection after this reply.  set on request and
	// updated after response from handler if there's a
	// "Connection: keep-alive" response header and a
	// Content-Length.
	closeAfterReply bool

	// When fullDuplex is false (the default), we consume any remaining
	// request body before starting to write a response.
	fullDuplex bool

	// requestBodyLimitHit is set by requestTooLarge when
	// maxBytesReader hits its max size. It is checked in
	// WriteHeader, to make sure we don't consume the
	// remaining request body to try to advance to the next HTTP
	// request. Instead, when this is set, we stop reading
	// subsequent requests on this connection and stop reading
	// input from it.
	requestBodyLimitHit bool

	// trailers are the headers to be sent after the handler
	// finishes writing the body. This field is initialized from
	// the Trailer response header when the response header is
	// written.
	trailers []string

	handlerDone atomic.Bool // set true when the handler exits

	// Buffers for Date, Content-Length, and status code
	dateBuf   [len(http.TimeFormat)]byte
	clenBuf   [10]byte
	statusBuf [3]byte

	// closeNotifyCh is the channel returned by CloseNotify.
	// non-nil. Make this lazily-created again as it used to be?
	closeNotifyCh  chan bool
	didCloseNotify atomic.Bool // atomic (only false->true winner should send)
}

// ListenAndServer it creates HTTP server that works with HTTP/1.1 protocol
func ListenAndServer(addr string, handler http.Handler) error {
	s := &MiniServer{Addr: addr, Handler: handler, SupportH2: false}

	return s.ListenAndServer()
}

// ListenAndServerTLS it creates HTTP server that has TLS in it works with HTTP/2.0 and HTTP/1.1
func ListenAndServerTLS(
	addr string, certFile, keyFile string,
	supportH2 bool,
	handler http.Handler) error {
	s := &MiniServer{
		Addr:      addr,
		Handler:   handler,
		SupportH2: supportH2,
	}

	return s.ListenAndServerTLS(certFile, keyFile)
}

func (s *MiniServer) ListenAndServer() error {
	addr := s.Addr

	if addr == "" {
		addr = ":http"
	}

	ln, err := net.Listen("tcp", addr)

	if err != nil {
		return err
	}

	return s.Serve(ln)
}

func (s *MiniServer) ListenAndServerTLS(certFile, keyFile string) error {
	addr := s.Addr
	if addr == "" {
		addr = ":https"
	}

	ln, err := net.Listen("tcp", addr)

	if err != nil {
		return err
	}

	return s.ServeTLS(ln, certFile, keyFile)
}

// onceCloseListener wraps a net.Listener, protecting it from
// multiple Close calls.
type onceCloseListener struct {
	net.Listener
	once     sync.Once
	closeErr error
}

func (oc *onceCloseListener) Close() error {
	oc.once.Do(oc.close)

	return oc.closeErr
}

func (oc *onceCloseListener) close() {
	oc.closeErr = oc.Listener.Close()
}

// MiniServer Methods

func (s *MiniServer) Serve(l net.Listener) error {
	if s.shuttingDown() {
		return http.ErrServerClosed
	}

	l = &onceCloseListener{Listener: l}

	defer l.Close()

	var tempDelay time.Duration // how long to sleep on accept failure

	ctx := context.WithValue(context.Background(), ServerContextKey, s)

	for {
		rw, err := l.Accept()
		if err != nil {
			if s.shuttingDown() {
				return http.ErrServerClosed
			}

			var ne net.Error

			//nolint:all
			if errors.As(err, &ne) && ne.Temporary() {

				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}

				if duration := 1 * time.Second; tempDelay > duration {
					tempDelay = duration
				}

				s.logf("http: Accept error: %v; retrying in %v", err, tempDelay)
				time.Sleep(tempDelay)
				continue
			}

			return err
		}

		tempDelay = 0
		c := s.newConn(rw)
		c.setState(StateNew) // before Serve can return

		go c.serve(ctx)
	}
}

func (s *MiniServer) ServeTLS(ln net.Listener, certFile, keyFile string) error {
	if s.shuttingDown() {
		return http.ErrServerClosed
	}

	var err error

	s.Certificate, err = tls.LoadX509KeyPair(certFile, keyFile)

	if err != nil {
		return err
	}

	return s.Serve(ln)
}

func (s *MiniServer) shuttingDown() bool {
	return s.inShutdown.Load()
}

func (s *MiniServer) logf(format string, args ...any) {
	if s.ErrorLog != nil {
		s.ErrorLog.Printf(format, args...)

		return
	}

	log.Printf(format, args...)
}

func (s *MiniServer) newConn(con net.Conn) *conn {
	return &conn{
		server: s,
		rwc:    con,
	}
}

// A ConnState represents the state of a client connection to a server.
// It's used by the optional Server.ConnState hook.
type ConnState int

const (
	// StateNew represents a new connection that is expected to
	// send a request immediately. Connections begin at this
	// state and then transition to either StateActive or
	// StateClosed.
	StateNew ConnState = iota

	// StateActive represents a connection that has read 1 or more
	// bytes of a request. The Server.ConnState hook for
	// StateActive fires before the request has entered a handler
	// and doesn't fire again until the request has been
	// handled. After the request is handled, the state
	// transitions to StateClosed, StateHijacked, or StateIdle.
	// For HTTP/2, StateActive fires on the transition from zero
	// to one active request, and only transitions away once all
	// active requests are complete. That means that ConnState
	// cannot be used to do per-request work; ConnState only notes
	// the overall state of the connection.
	StateActive

	// StateIdle represents a connection that has finished
	// handling a request and is in the keep-alive state, waiting
	// for a new request. Connections transition from StateIdle
	// to either StateActive or StateClosed.
	StateIdle

	// StateClosed represents a closed connection.
	// This is a terminal state. Hijacked connections do not
	// transition to StateClosed.
	StateClosed
)

var stateName = map[ConnState]string{
	StateNew:    "new",
	StateActive: "active",
	StateIdle:   "idle",
	StateClosed: "closed",
}

func (c ConnState) String() string {
	return stateName[c]
}

func (s *MiniServer) trackConn(c *conn, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.activeConn == nil {
		s.activeConn = make(map[*conn]struct{})
	}

	if add {
		s.activeConn[c] = struct{}{}
		return
	}

	delete(s.activeConn, c)
}

// DefaultMaxHeaderBytes is the maximum permitted size of the headers
// in an HTTP request.
// This can be overridden by setting Server.MaxHeaderBytes.
const DefaultMaxHeaderBytes = 1 << 20 // 1 MB

func (s *MiniServer) maxHeaderBytes() int {
	if s.MaxHeaderBytes > 0 {
		return s.MaxHeaderBytes
	}

	return DefaultMaxHeaderBytes
}

func (s *MiniServer) initialReadLimitSize() int64 {
	return int64(s.maxHeaderBytes()) + 4096 // bufio slop
}

// conn Methods

func (c *conn) setState(state ConnState) {
	srv := c.server

	switch state {
	case StateNew:
		srv.trackConn(c, true)
	case StateClosed:
		srv.trackConn(c, false)
	}

	if state > 0xff || state < 0 {
		log.Panicln("internal error")
	}

	packedState := uint64(time.Now().Unix()<<8) | uint64(state)

	c.curState.Store(packedState)
}

func (c *conn) serve(ctx context.Context) {
	if ra := c.rwc.RemoteAddr(); ra != nil {
		c.remoteAddr = ra.String()
	}

	ctx = context.WithValue(ctx, LocalAddrContextKey, c.rwc.LocalAddr())

	var inFlightResponse *response

	defer func() {
		//nolint:all
		if err := recover(); err != nil && err != http.ErrAbortHandler {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			c.server.logf("http: panic serving %v: %v\n%s", c.remoteAddr, err, buf)
		}

		if inFlightResponse != nil {
			inFlightResponse.cancelCtx()
		}

		if inFlightResponse != nil {
			inFlightResponse.conn.r.abortPendingRead()
			inFlightResponse.reqBody.Close()
		}

		c.close()
		c.setState(StateClosed)
	}()

	if tlsConn, ok := c.rwc.(*tls.Conn); ok {
		fmt.Println(tlsConn)
	}

	// HTTP/1.x from here on

	ctx, cancelCtx := context.WithCancel(ctx)
	c.cancelCtx = cancelCtx
	defer cancelCtx()

	c.r = &connReader{conn: c}
	c.bufr = newBufioReader(c.r)
	c.bufw = newBufioWriterSize(checkConnErrorWriter{c}, 4<<10)

	for {
		w, err := c.readRequest(ctx)

		if c.r.remain != c.server.initialReadLimitSize() {
			// If we read any bytes off the wire, we're active.
			c.setState(StateActive)
		}

		if err != nil {
			fmt.Println(err, w)
		}
	}
}

var errTooLarge = errors.New("http: request too large")

// Read next request from connection.
func (c *conn) readRequest(ctx context.Context) (w *response, err error) {
	return nil, err
}

func (c *conn) close() {
	c.finalFlush()
	c.rwc.Close()
}

func newBufioReader(r io.Reader) *bufio.Reader {
	if v := bufioReaderPool.Get(); v != nil {
		br := v.(*bufio.Reader)
		br.Reset(r)

		return br
	}

	// Note: if this reader size is ever changed, update
	// TestHandlerBodyClose's assumptions.
	return bufio.NewReader(r)
}

func putBufioReader(br *bufio.Reader) {
	br.Reset(nil)
	bufioReaderPool.Put(br)
}

func newBufioWriterSize(w io.Writer, size int) *bufio.Writer {
	pool := bufioWriterPool(size)

	if pool != nil {
		if v := pool.Get(); v != nil {
			bw := v.(*bufio.Writer)
			bw.Reset(w)

			return bw
		}
	}

	return bufio.NewWriterSize(w, size)
}

// bufioWriterPool it checks the available size so it can assign correct writer to it
func bufioWriterPool(size int) *sync.Pool {
	switch size {
	case 2 << 10:
		return &bufioWriter2kPool
	case 4 << 10:
		return &bufioWriter4kPool
	}

	return nil
}

func putBufioWriter(bw *bufio.Writer) {
	bw.Reset(nil)

	if pool := bufioWriterPool(bw.Available()); pool != nil {
		pool.Put(bw)
	}
}

// checkConnErrorWriter writes to c.rwc and records any write errors to c.werr.
// It only contains one field (and a pointer field at that), so it
// fits in an interface value without an extra allocation.
type checkConnErrorWriter struct {
	c *conn
}

func (w checkConnErrorWriter) Write(p []byte) (n int, err error) {
	n, err = w.c.rwc.Write(p)

	if err != nil && w.c.werr == nil {
		w.c.werr = err
		w.c.cancelCtx()
	}

	return
}

func (c *conn) finalFlush() {
	if c.bufr != nil {
		// Steal the bufio.Reader (~4KB worth of memory) and its associated
		// reader for a future connection.
		putBufioReader(c.bufr)
		c.bufr = nil
	}

	if c.bufw != nil {
		c.bufw.Flush()
		// Steal the bufio.Writer (~4KB worth of memory) and its associated
		// writer for a future connection.
		putBufioWriter(c.bufw)
		c.bufw = nil
	}
}

// connReader methods

// connReader is the io.Reader wrapper used by *conn. It combines a
// selectively-activated io.LimitedReader (to bound request header
// read sizes) with support for selectively keeping an io.Reader.Read
// call blocked in a background goroutine to wait for activity and
// trigger a CloseNotifier channel.
type connReader struct {
	conn *conn

	mu      sync.Mutex // guards following
	hasByte bool
	byteBuf [1]byte
	cond    *sync.Cond
	inRead  bool
	aborted bool  // set true before conn.rwc deadline is set to past
	remain  int64 // bytes remaining
}

func (cr *connReader) lock() {
	cr.mu.Lock()

	if cr.cond == nil {
		cr.cond = sync.NewCond(&cr.mu)
	}
}

func (cr *connReader) unlock() { cr.mu.Unlock() }

func (cr *connReader) startBackgroundRead() {
	cr.lock()
	defer cr.unlock()

	if cr.inRead {
		panic("invalid concurrent Body.Read call")
	}

	if cr.hasByte {
		return
	}

	cr.inRead = true

	//nolint:all
	cr.conn.rwc.SetReadDeadline(time.Time{})

	go cr.backgroundRead()
}

func (cr *connReader) backgroundRead() {
	n, err := cr.conn.rwc.Read(cr.byteBuf[:])

	cr.lock()

	if n == 1 {
		cr.hasByte = true
	}

	var ne net.Error

	//nolint:all
	if errors.As(err, &ne) && cr.aborted && ne.Timeout() {
		// Ignore this error. It's the expected error from
		// another goroutine calling abortPendingRead.
	}

	cr.aborted = false
	cr.inRead = false
	cr.unlock()
	cr.cond.Broadcast()
}

func (cr *connReader) abortPendingRead() {
	cr.lock()
	defer cr.unlock()

	if !cr.inRead {
		return
	}

	cr.aborted = true

	//nolint:all
	cr.conn.rwc.SetReadDeadline(aLongTimeAgo)

	for cr.inRead {
		cr.cond.Wait()
	}

	//nolint:all
	cr.conn.rwc.SetReadDeadline(time.Time{})
}

func (cr *connReader) setReadLimit(remain int64) {
	cr.remain = remain
}

func (cr *connReader) setInfiniteReadLimit() {
	cr.remain = maxInt64
}

func (cr *connReader) hitReadLimit() bool {
	return cr.remain <= 0
}

// handleReadError is called whenever a Read from the client returns a
// non-nil error.
//
// The provided non-nil err is almost always io.EOF or a "use of
// closed network connection". In any case, the error is not
// particularly interesting, except perhaps for debugging during
// development. Any error means the connection is dead and we should
// down its context.
//
// It may be called from multiple goroutines.
func (cr *connReader) handleReadError(_ error) {
	cr.conn.cancelCtx()
	cr.closeNotify()
}

// may be called from multiple goroutines.
func (cr *connReader) closeNotify() {
	res := cr.conn.curReq.Load()

	if res != nil && !res.didCloseNotify.Swap(true) {
		res.closeNotifyCh <- true
	}
}

func (cr *connReader) Read(p []byte) (n int, err error) {
	cr.lock()

	if cr.inRead {
		cr.unlock()
		panic("invalid concurrent Body.Read call")
	}

	if cr.hitReadLimit() {
		cr.unlock()
		return 0, io.EOF
	}

	if len(p) == 0 {
		cr.unlock()
		return 0, nil
	}

	if int64(len(p)) > cr.remain {
		p = p[:cr.remain]
	}

	if cr.hasByte {
		p[0] = cr.byteBuf[0]
		cr.hasByte = false
		cr.unlock()

		return 1, nil
	}

	cr.inRead = true
	cr.unlock()
	n, err = cr.conn.rwc.Read(p)

	cr.lock()
	cr.inRead = false

	if err != nil {
		cr.handleReadError(err)
	}

	cr.remain -= int64(n)
	cr.unlock()

	cr.cond.Broadcast()

	return n, err
}
