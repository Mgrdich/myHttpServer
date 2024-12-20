package pkg

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/textproto"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/http/httpguts"
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

var (
	crlf       = []byte("\r\n")
	colonSpace = []byte(": ")
)

var (
	suppressedHeaders304    = []string{"Content-Type", "Content-Length", "Transfer-Encoding"}
	suppressedHeadersNoBody = []string{"Content-Length", "Transfer-Encoding"}
	excludedHeadersNoBody   = map[string]bool{"Content-Length": true, "Transfer-Encoding": true}
)

// Sorted the same as extraHeader.Write's loop.
var extraHeaderKeys = [][]byte{
	[]byte("Content-Type"),
	[]byte("Connection"),
	[]byte("Transfer-Encoding"),
}

var (
	headerContentLength = []byte("Content-Length: ")
	headerDate          = []byte("Date: ")
)

var errTooLarge = errors.New("http: request too large")

const closeStr = "close"

// rstAvoidanceDelay is the amount of time we sleep after closing the
// write side of a TCP connection before closing the entire socket.
// By sleeping, we increase the chances that the client sees our FIN
// and processes its final data before they process the subsequent RST
// from closing a connection with known unread data.
// This RST seems to occur mostly on BSD systems. (And Windows?)
// This timeout is somewhat arbitrary (~latency around the planet).
const rstAvoidanceDelay = 500 * time.Millisecond

// This should be >= 512 bytes for DetectContentType,
// but otherwise it's somewhat arbitrary.
const bufferBeforeChunkingSize = 2048

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

	// tlsState is the TLS connection state when using TLS.
	// nil means not TLS.
	tlsState *tls.ConnectionState

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

	// TLSConfig optionally provides a TLS configuration for use
	// by ServeTLS and ListenAndServeTLS. Note that this value is
	// cloned by ServeTLS and ListenAndServeTLS, so it's not
	// possible to modify the configuration with methods like
	// tls.Config.SetSessionTicketKeys. To use
	// SetSessionTicketKeys, use Server.Serve with a TLS Listener
	// instead.
	TLSConfig *tls.Config

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

	// TLSNextProto optionally specifies a function to take over
	// ownership of the provided TLS connection when an ALPN
	// protocol upgrade has occurred. The map key is the protocol
	// name negotiated. The Handler argument should be used to
	// handle HTTP requests and will initialize the Request's TLS
	// and RemoteAddr if not already set. The connection is
	// automatically closed when the function returns.
	// If TLSNextProto is not nil, HTTP/2 support is not enabled
	// automatically.
	TLSNextProto map[string]func(*MiniServer, *tls.Conn, http.Handler)

	nextProtoOnce sync.Once // guards setupHTTP2_* init
	nextProtoErr  error     // result of http2.ConfigureServer if used

	disableKeepAlives atomic.Bool

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

// foreachHeaderElement splits v according to the "#rule" construction
// in RFC 7230 section 7 and calls fn for each non-empty element.
func foreachHeaderElement(v string, fn func(string)) {
	v = textproto.TrimString(v)
	if v == "" {
		return
	}

	if !strings.Contains(v, ",") {
		fn(v)
		return
	}

	for _, f := range strings.Split(v, ",") {
		if f = textproto.TrimString(f); f != "" {
			fn(f)
		}
	}
}

func suppressedHeaders(status int) []string {
	switch {
	case status == 304:
		// RFC 7232 section 4.1
		return suppressedHeaders304
	case !bodyAllowedForStatus(status):
		return suppressedHeadersNoBody
	}

	return nil
}

// appendTime is a non-allocating version of []byte(t.UTC().Format(TimeFormat))
func appendTime(b []byte, t time.Time) []byte {
	const days = "SunMonTueWedThuFriSat"

	const months = "JanFebMarAprMayJunJulAugSepOctNovDec"

	t = t.UTC()
	yy, mm, dd := t.Date()
	hh, mn, ss := t.Clock()
	day := days[3*t.Weekday():]
	mon := months[3*(mm-1):]

	return append(b,
		day[0], day[1], day[2], ',', ' ',
		byte('0'+dd/10), byte('0'+dd%10), ' ',
		mon[0], mon[1], mon[2], ' ',
		byte('0'+yy/1000), byte('0'+(yy/100)%10), byte('0'+(yy/10)%10), byte('0'+yy%10), ' ',
		byte('0'+hh/10), byte('0'+hh%10), ':',
		byte('0'+mn/10), byte('0'+mn%10), ':',
		byte('0'+ss/10), byte('0'+ss%10), ' ',
		'G', 'M', 'T')
}

// writeHeader finalizes the header sent to the client and writes it
// to cw.res.conn.bufw.
//
// p is not written by writeHeader, but is the first chunk of the body
// that will be written. It is sniffed for a Content-Type if none is
// set explicitly. It's also used to set the Content-Length, if the
// total body size was small and the handler has already finished
// running.
func (cw *chunkWriter) writeHeader(p []byte) {
	if cw.wroteHeader {
		return
	}

	cw.wroteHeader = true

	w := cw.res
	keepAlivesEnabled := w.conn.server.doKeepAlives()
	isHEAD := w.req.Method == http.MethodHead

	// header is written out to w.conn.buf below. Depending on the
	// state of the handler, we either own the map or not. If we
	// don't own it, the exclude map is created lazily for
	// WriteSubset to remove headers. The setHeader struct holds
	// headers we need to add.
	header := cw.header
	owned := header != nil

	if !owned {
		header = w.handlerHeader
	}

	var excludeHeader map[string]bool

	delHeader := func(key string) {
		if owned {
			header.Del(key)

			return
		}

		if _, ok := header[key]; !ok {
			return
		}

		if excludeHeader == nil {
			excludeHeader = make(map[string]bool)
		}

		excludeHeader[key] = true
	}

	var setHeader extraHeader

	// Don't write out the fake "Trailer:foo" keys. See TrailerPrefix.
	trailers := false

	for k := range cw.header {
		if strings.HasPrefix(k, http.TrailerPrefix) {
			if excludeHeader == nil {
				excludeHeader = make(map[string]bool)
			}

			excludeHeader[k] = true
			trailers = true
		}
	}

	for _, v := range cw.header["Trailer"] {
		trailers = true

		foreachHeaderElement(v, cw.res.declareTrailer)
	}

	te := GetHeader(header, "Transfer-Encoding")
	hasTE := te != ""

	// If the handler is done but never sent a Content-Length
	// response header and this is our first (and last) write, set
	// it, even to zero. This helps HTTP/1.0 clients keep their
	// "keep-alive" connections alive.
	// Exceptions: 304/204/1xx responses never get Content-Length, and if
	// it was a HEAD request, we don't know the difference between
	// 0 actual bytes and 0 bytes because the handler noticed it
	// was a HEAD request and chose not to write anything. So for
	// HEAD, the handler should either write the Content-Length or
	// write non-zero bytes. If it's actually 0 bytes and the
	// handler never looked at the Request.Method, we just don't
	// send a Content-Length header.
	// Further, we don't send an automatic Content-Length if they
	// set a Transfer-Encoding, because they're generally incompatible.
	if w.handlerDone.Load() &&
		!trailers &&
		!hasTE &&
		bodyAllowedForStatus(w.status) &&
		!HasHeader(header, "Content-Length") && (!isHEAD || len(p) > 0) {
		w.contentLength = int64(len(p))
		setHeader.contentLength = strconv.AppendInt(cw.res.clenBuf[:0], int64(len(p)), 10)
	}

	// If this was an HTTP/1.0 request with keep-alive and we sent a
	// Content-Length back, we can make this a keep-alive response ...
	if w.wants10KeepAlive && keepAlivesEnabled {
		sentLength := GetHeader(header, "Content-Length") != ""
		if sentLength && GetHeader(header, "Connection") == "keep-alive" {
			w.closeAfterReply = false
		}
	}

	// Check for an explicit (and valid) Content-Length header.
	hasCL := w.contentLength != -1

	if w.wants10KeepAlive && (isHEAD || hasCL || !bodyAllowedForStatus(w.status)) {
		_, connectionHeaderSet := header["Connection"]
		if !connectionHeaderSet {
			setHeader.connection = "keep-alive"
		}
	} else if !w.req.ProtoAtLeast(1, 1) || w.wantsClose {
		w.closeAfterReply = true
	}

	if GetHeader(header, "Connection") == closeStr || !keepAlivesEnabled {
		w.closeAfterReply = true
	}

	// If the client wanted a 100-continue but we never sent it to
	// them (or, more strictly: we never finished reading their
	// request body), don't reuse this connection because it's now
	// in an unknown state: we might be sending this response at
	// the same time the client is now sending its request body
	// after a timeout.  (Some HTTP clients send Expect:
	// 100-continue but knowing that some servers don't support
	// it, the clients set a timer and send the body later anyway)
	// If we haven't seen EOF, we can't skip over the unread body
	// because we don't know if the next bytes on the wire will be
	// the body-following-the-timer or the subsequent request.
	// See Issue 11549.
	if ecr, ok := w.req.Body.(*expectContinueReader); ok && !ecr.sawEOF.Load() {
		w.closeAfterReply = true
	}

	// We do this by default because there are a number of clients that
	// send a full request before starting to read the response, and they
	// can deadlock if we start writing the response with unconsumed body
	// remaining. See Issue 15527 for some history.
	//
	// If full duplex mode has been enabled with ResponseController.EnableFullDuplex,
	// then leave the request body alone.
	if w.req.ContentLength != 0 && !w.closeAfterReply && !w.fullDuplex {
		var discard, tooBig bool

		switch bdy := w.req.Body.(type) {
		case *expectContinueReader:
			if bdy.resp.wroteContinue {
				discard = true
			}
		case *body:
			bdy.mu.Lock()

			switch {
			case bdy.closed:
				if !bdy.sawEOF {
					// Body was closed in handler with non-EOF error.
					w.closeAfterReply = true
				}
			case bdy.unreadDataSizeLocked() >= maxPostHandlerReadBytes:
				tooBig = true
			default:
				discard = true
			}
			bdy.mu.Unlock()
		default:
			discard = true
		}

		if discard {
			_, err := io.CopyN(io.Discard, w.reqBody, maxPostHandlerReadBytes+1)

			switch {
			case err == nil:
				// There must be even more data left over.
				tooBig = true
			case errors.Is(err, http.ErrBodyReadAfterClose):
				// Body was already consumed and closed.
			case errors.Is(err, io.EOF):
				// The remaining body was just consumed, close it.
				err = w.reqBody.Close()
				if err != nil {
					w.closeAfterReply = true
				}
			default:
				// Some other kind of error occurred, like a read timeout, or
				// corrupt chunked encoding. In any case, whatever remains
				// on the wire must not be parsed as another HTTP request.
				w.closeAfterReply = true
			}
		}

		if tooBig {
			w.requestTooLarge()

			delHeader("Connection")

			setHeader.connection = closeStr
		}
	}

	code := w.status
	if bodyAllowedForStatus(code) {
		// If no content type, apply sniffing algorithm to body.
		_, haveType := header["Content-Type"]

		// If the Content-Encoding was set and is non-blank,
		// we shouldn't sniff the body. See Issue 31753.
		ce := header.Get("Content-Encoding")
		hasCE := len(ce) > 0

		if !hasCE && !haveType && !hasTE && len(p) > 0 {
			setHeader.contentType = http.DetectContentType(p)
		}
	} else {
		for _, k := range suppressedHeaders(code) {
			delHeader(k)
		}
	}

	if !HasHeader(header, "Date") {
		setHeader.date = appendTime(cw.res.dateBuf[:0], time.Now())
	}

	if hasCL && hasTE && te != "identity" {
		// TODO: return an error if WriteHeader gets a return parameter
		// For now just ignore the Content-Length.
		w.conn.server.logf("http: WriteHeader called with both Transfer-Encoding of %q and a Content-Length of %d",
			te, w.contentLength)
		delHeader("Content-Length")

		hasCL = false
	}

	if w.req.Method == http.MethodHead || !bodyAllowedForStatus(code) || code == http.StatusNoContent {
		// Response has no body.
		delHeader("Transfer-Encoding")
	} else if hasCL {
		// Content-Length has been provided, so no chunking is to be done.
		delHeader("Transfer-Encoding")
	} else if w.req.ProtoAtLeast(1, 1) {
		// HTTP/1.1 or greater: Transfer-Encoding has been set to identity, and no
		// content-length has been provided. The connection must be closed after the
		// reply is written, and no chunking is to be done. This is the setup
		// recommended in the Server-Sent Events candidate recommendation 11,
		// section 8.
		if hasTE && te == "identity" {
			cw.chunking = false
			w.closeAfterReply = true

			delHeader("Transfer-Encoding")
		} else {
			// HTTP/1.1 or greater: use chunked transfer encoding
			// to avoid closing the connection at EOF.
			cw.chunking = true
			setHeader.transferEncoding = "chunked"

			if hasTE && te == "chunked" {
				// We will send the chunked Transfer-Encoding header later.
				delHeader("Transfer-Encoding")
			}
		}
	} else {
		// HTTP version < 1.1: cannot do chunked transfer
		// encoding and we don't know the Content-Length so
		// signal EOF by closing connection.
		w.closeAfterReply = true

		delHeader("Transfer-Encoding") // in case already set
	}

	// Cannot use Content-Length with non-identity Transfer-Encoding.
	if cw.chunking {
		delHeader("Content-Length")
	}

	if !w.req.ProtoAtLeast(1, 0) {
		return
	}

	// Only override the Connection header if it is not a successful
	// protocol switch response and if KeepAlives are not enabled.
	// See https://golang.org/issue/36381.
	delConnectionHeader := w.closeAfterReply &&
		(!keepAlivesEnabled || !hasToken(GetHeader(cw.header, "Connection"), closeStr)) &&
		!isProtocolSwitchResponse(w.status, header)
	if delConnectionHeader {
		delHeader("Connection")

		if w.req.ProtoAtLeast(1, 1) {
			setHeader.connection = closeStr
		}
	}

	writeStatusLine(w.conn.bufw, w.req.ProtoAtLeast(1, 1), code, w.statusBuf[:])
	//nolint:errcheck
	cw.header.WriteSubset(w.conn.bufw, excludeHeader)
	setHeader.Write(w.conn.bufw)
	//nolint:errcheck
	w.conn.bufw.Write(crlf)
}

func (cw *chunkWriter) Write(p []byte) (n int, err error) {
	if !cw.wroteHeader {
		cw.writeHeader(p)
	}

	if cw.res.req.Method == http.MethodHead {
		return len(p), nil
	}

	if cw.chunking {
		_, err = fmt.Fprintf(cw.res.conn.bufw, "%x\r\n", len(p))
		if err != nil {
			cw.res.conn.rwc.Close()
			return
		}
	}

	n, err = cw.res.conn.bufw.Write(p)
	if cw.chunking && err == nil {
		_, err = cw.res.conn.bufw.Write(crlf)
	}

	if err != nil {
		cw.res.conn.rwc.Close()
	}

	return
}

func (cw *chunkWriter) flush() error {
	if !cw.wroteHeader {
		cw.writeHeader(nil)
	}

	return cw.res.conn.bufw.Flush()
}

func (cw *chunkWriter) close() {
	if !cw.wroteHeader {
		cw.writeHeader(nil)
	}

	if cw.chunking {
		bw := cw.res.conn.bufw // conn's bufio writer
		// zero chunk to mark EOF
		//nolint:errcheck
		bw.WriteString("0\r\n")

		if trailers := cw.res.finalTrailers(); trailers != nil {
			//nolint:errcheck
			trailers.Write(bw) // the writer handles noting errors
		}
		// final blank line after the trailers (whether
		// present or not)
		//nolint:errcheck
		bw.WriteString("\r\n")
	}
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

// ListenAndServe it creates HTTP server that works with HTTP/1.1 protocol
func ListenAndServe(addr string, handler http.Handler) error {
	if handler == nil {
		log.Panic("handler can't be nil")
	}

	s := &MiniServer{Addr: addr, Handler: handler, SupportH2: false}

	return s.ListenAndServer()
}

// ListenAndServeTLS it creates HTTP server that has TLS in it works with HTTP/2.0 and HTTP/1.1
func ListenAndServeTLS(
	addr string,
	certFile, keyFile string,
	supportH2 bool,
	handler http.Handler) error {
	if handler == nil {
		log.Panic("handler can't be nil")
	}

	s := &MiniServer{
		Addr:      addr,
		Handler:   handler,
		SupportH2: supportH2,
	}

	return s.ListenAndServerTLS(certFile, keyFile)
}

// cloneTLSConfig returns a shallow clone of cfg, or a new zero tls.Config if
// cfg is nil. This is safe to call even if cfg is in active use by a TLS
// client or server.
func cloneTLSConfig(cfg *tls.Config) *tls.Config {
	if cfg == nil {
		//nolint:gosec
		return &tls.Config{}
	}

	return cfg.Clone()
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

// onceSetNextProtoDefaults configures HTTP/2, if the user hasn't
// configured otherwise. (by setting srv.TLSNextProto non-nil)
// It must only be called via srv.nextProtoOnce (use srv.setupHTTP2_*).
func (s *MiniServer) onceSetNextProtoDefaults() {
	if !s.SupportH2 {
		return
	}

	// Enable HTTP/2 by default if the user hasn't otherwise
	// configured their TLSNextProto map.
	if s.TLSNextProto == nil {
		//conf := &http2Server{
		//	NewWriteScheduler: func() http2WriteScheduler { return http2NewPriorityWriteScheduler(nil) },
		//}
		//s.nextProtoErr = http2ConfigureServer(srv, conf)
		log.Println("http2 not implemented yet sorry")
	}
}

// setupHTTP2_ServeTLS conditionally configures HTTP/2 on
// srv and reports whether there was an error setting it up. If it is
// not configured for policy reasons, nil is returned.
func (s *MiniServer) setupHTTP2ServeTLS() error {
	s.nextProtoOnce.Do(s.onceSetNextProtoDefaults)

	return s.nextProtoErr
}

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
	// Setup HTTP/2 before srv.Serve, to initialize srv.TLSConfig
	// before we clone it and create the TLS Listener.
	if err := s.setupHTTP2ServeTLS(); err != nil {
		return err
	}

	config := cloneTLSConfig(s.TLSConfig)
	if !strSliceContains(config.NextProtos, "http/1.1") {
		config.NextProtos = append(config.NextProtos, "http/1.1")
	}

	var err error

	config.Certificates = make([]tls.Certificate, 1)
	config.Certificates[0], err = tls.LoadX509KeyPair(certFile, keyFile)

	if err != nil {
		return err
	}

	tlsListener := tls.NewListener(ln, config)

	return s.Serve(tlsListener)
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

func (s *MiniServer) doKeepAlives() bool {
	return !s.disableKeepAlives.Load() && !s.shuttingDown()
}

func (s *MiniServer) idleTimeout() time.Duration {
	if s.IdleTimeout != 0 {
		return s.IdleTimeout
	}

	return s.ReadTimeout
}

func (s *MiniServer) readHeaderTimeout() time.Duration {
	if s.ReadHeaderTimeout != 0 {
		return s.ReadHeaderTimeout
	}

	return s.ReadTimeout
}

// tlsHandshakeTimeout returns the time limit permitted for the TLS
// handshake, or zero for unlimited.
//
// It returns the minimum of any positive ReadHeaderTimeout,
// ReadTimeout, or WriteTimeout.
func (s *MiniServer) tlsHandshakeTimeout() time.Duration {
	var ret time.Duration

	for _, v := range [...]time.Duration{
		s.ReadHeaderTimeout,
		s.ReadTimeout,
		s.WriteTimeout,
	} {
		if v <= 0 {
			continue
		}

		if ret == 0 || v < ret {
			ret = v
		}
	}

	return ret
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

// response Methods
// maxPostHandlerReadBytes is the max number of Request.Body bytes not
// consumed by a handler that the server will read from the client
// in order to keep a connection alive. If there are more bytes than
// this then the server to be paranoid instead sends a "Connection:
// close" response.
//
// This number is approximately what a typical machine's TCP buffer
// size is anyway.  (if we have the bytes on the machine, we might as
// well read them)
const maxPostHandlerReadBytes = 256 << 10

func checkWriteHeaderCode(code int) {
	// Issue 22880: require valid WriteHeader status codes.
	// For now we only enforce that it's three digits.
	// In the future we might block things over 599 (600 and above aren't defined
	// at https://httpwg.org/specs/rfc7231.html#status.codes).
	// But for now any three digits.
	//
	// We used to send "HTTP/1.1 000 0" on the wire in responses but there's
	// no equivalent bogus thing we can realistically send in HTTP/2,
	// so we'll consistently panic instead and help people find their bugs
	// early. (We can't return an error from WriteHeader even if we wanted to.)
	if code < 100 || code > 999 {
		panic(fmt.Sprintf("invalid WriteHeader code %v", code))
	}
}

func (w *response) sendExpectationFailed() {
	// requests with non-standard expectation[s]? Seems
	// theoretical at best, and doesn't fit into the
	// current ServeHTTP model anyway. We'd need to
	// make the ResponseWriter an optional
	// "ExpectReplier" interface or something.
	//
	// For now we'll just obey RFC 7231 5.1.1 which says
	// "A server that receives an Expect field-value other
	// than 100-continue MAY respond with a 417 (Expectation
	// Failed) status code to indicate that the unexpected
	// expectation cannot be met."
	w.Header().Set("Connection", closeStr)
	w.WriteHeader(http.StatusExpectationFailed)
	w.finishRequest()
}

func (w *response) Header() http.Header {
	if w.cw.header == nil && w.wroteHeader && !w.cw.wroteHeader {
		// Accessing the header between logically writing it
		// and physically writing it means we need to allocate
		// a clone to snapshot the logically written state.
		w.cw.header = w.handlerHeader.Clone()
	}

	w.calledHeader = true

	return w.handlerHeader
}

// declareTrailer is called for each Trailer header when the
// response header is written. It notes that a header will need to be
// written in the trailers at the end of the response.
func (w *response) declareTrailer(k string) {
	k = http.CanonicalHeaderKey(k)
	if !httpguts.ValidTrailerHeader(k) {
		// Forbidden by RFC 7230, section 4.1.2
		return
	}

	w.trailers = append(w.trailers, k)
}

// requestTooLarge is called by maxBytesReader when too much input has
// been read from the client.
func (w *response) requestTooLarge() {
	w.closeAfterReply = true
	w.requestBodyLimitHit = true

	if !w.wroteHeader {
		w.Header().Set("Connection", closeStr)
	}
}

// bodyAllowed reports whether a Write is allowed for this response type.
// It's illegal to call this before the header has been flushed.
func (w *response) bodyAllowed() bool {
	if !w.wroteHeader {
		panic("")
	}

	return bodyAllowedForStatus(w.status)
}

// The Life Of A Write is like this:
//
// Handler starts. No header has been sent. The handler can either
// write a header, or just start writing. Writing before sending a header
// sends an implicitly empty 200 OK header.
//
// If the handler didn't declare a Content-Length up front, we either
// go into chunking mode or, if the handler finishes running before
// the chunking buffer size, we compute a Content-Length and send that
// in the header instead.
//
// Likewise, if the handler didn't set a Content-Type, we sniff that
// from the initial chunk of output.
//
// The Writers are wired together like:
//
//  1. *response (the ResponseWriter) ->
//  2. (*response).w, a *bufio.Writer of bufferBeforeChunkingSize bytes ->
//  3. chunkWriter.Writer (whose writeHeader finalizes Content-Length/Type)
//     and which writes the chunk headers, if needed ->
//  4. conn.bufw, a *bufio.Writer of default (4kB) bytes, writing to ->
//  5. checkConnErrorWriter{c}, which notes any non-nil error on Write
//     and populates c.werr with it if so, but otherwise writes to ->
//  6. the rwc, the net.Conn.
//

// initial header contains both a Content-Type and Content-Length.
// Also short-circuit in (1) when the header's been sent and not in
// chunking mode, writing directly to (4) instead, if (2) has no
// buffered data. More generally, we could short-circuit from (1) to
// (3) even in chunking mode if the write size from (1) is over some
// threshold and nothing is in (2).  The answer might be mostly making
// bufferBeforeChunkingSize smaller and having bufio's fast-paths deal
// with this instead.
func (w *response) Write(data []byte) (n int, err error) {
	return w.write(len(data), data, "")
}

func (w *response) WriteString(data string) (n int, err error) {
	return w.write(len(data), nil, data)
}

// either dataB or dataS is non-zero.
func (w *response) write(lenData int, dataB []byte, dataS string) (n int, err error) {
	if w.canWriteContinue.Load() {
		// Body reader wants to write 100 Continue but hasn't yet.
		// Tell it not to. The store must be done while holding the lock
		// because the lock makes sure that there is not an active write
		// this very moment.
		w.writeContinueMu.Lock()
		w.canWriteContinue.Store(false)
		w.writeContinueMu.Unlock()
	}

	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}

	if lenData == 0 {
		return 0, nil
	}

	if !w.bodyAllowed() {
		return 0, http.ErrBodyNotAllowed
	}

	w.written += int64(lenData) // ignoring errors, for errorKludge
	if w.contentLength != -1 && w.written > w.contentLength {
		return 0, http.ErrContentLength
	}

	if dataB != nil {
		return w.w.Write(dataB)
	} else {
		return w.w.WriteString(dataS)
	}
}

// writeStatusLine writes an HTTP/1.x Status-Line (RFC 7230 Section 3.1.2)
// to bw. is11 is whether the HTTP request is HTTP/1.1. false means HTTP/1.0.
// code is the response status code.
// scratch is an optional scratch buffer. If it has at least capacity 3, it's used.
func writeStatusLine(bw *bufio.Writer, is11 bool, code int, scratch []byte) {
	if is11 {
		//nolint:errcheck
		bw.WriteString("HTTP/1.1 ")
	} else {
		//nolint:errcheck
		bw.WriteString("HTTP/1.0 ")
	}

	if text := http.StatusText(code); text != "" {
		//nolint:errcheck
		bw.Write(strconv.AppendInt(scratch[:0], int64(code), 10))
		//nolint:errcheck
		bw.WriteByte(' ')
		//nolint:errcheck
		bw.WriteString(text)
		//nolint:errcheck
		bw.WriteString("\r\n")
	} else {
		// don't worry about performance
		fmt.Fprintf(bw, "%03d status code %d\r\n", code, code)
	}
}

func (w *response) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}

	checkWriteHeaderCode(code)

	// Handle informational headers.
	//
	// We shouldn't send any further headers after 101 Switching Protocols,
	// so it takes the non-informational path.
	if code >= 100 && code <= 199 && code != http.StatusSwitchingProtocols {
		// Prevent a potential race with an automatically-sent 100 Continue triggered by Request.Body.Read()
		if code == 100 && w.canWriteContinue.Load() {
			w.writeContinueMu.Lock()
			w.canWriteContinue.Store(false)
			w.writeContinueMu.Unlock()
		}

		writeStatusLine(w.conn.bufw, w.req.ProtoAtLeast(1, 1), code, w.statusBuf[:])

		// Per RFC 8297 we must not clear the current header map
		//nolint:all
		w.handlerHeader.WriteSubset(w.conn.bufw, excludedHeadersNoBody)
		//nolint:all
		w.conn.bufw.Write(crlf)
		//nolint:all
		w.conn.bufw.Flush()

		return
	}

	w.wroteHeader = true
	w.status = code

	if w.calledHeader && w.cw.header == nil {
		w.cw.header = w.handlerHeader.Clone()
	}

	if cl := GetHeader(w.handlerHeader, "Content-Length"); cl != "" {
		v, err := strconv.ParseInt(cl, 10, 64)
		if err == nil && v >= 0 {
			w.contentLength = v
		} else {
			w.conn.server.logf("http: invalid Content-Length of %q", cl)
			w.handlerHeader.Del("Content-Length")
		}
	}
}

func (w *response) finishRequest() {
	w.handlerDone.Store(true)

	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}

	w.w.Flush()
	putBufioWriter(w.w)
	w.cw.close()
	w.conn.bufw.Flush()

	w.conn.r.abortPendingRead()

	// Close the body (regardless of w.closeAfterReply) so we can
	// re-use its bufio.Reader later safely.
	w.reqBody.Close()

	if w.req.MultipartForm != nil {
		//nolint:errcheck
		w.req.MultipartForm.RemoveAll()
	}
}

func (w *response) closedRequestBodyEarly() bool {
	body, ok := w.req.Body.(*body)

	return ok && body.didEarlyClose()
}

// shouldReuseConnection reports whether the underlying TCP connection can be reused.
// It must only be called after the handler is done executing.
func (w *response) shouldReuseConnection() bool {
	if w.closeAfterReply {
		// The request or something set while executing the
		// handler indicated we shouldn't reuse this
		// connection.
		return false
	}

	if w.req.Method != http.MethodHead && w.contentLength != -1 && w.bodyAllowed() && w.contentLength != w.written {
		// Did not write enough. Avoid getting out of sync.
		return false
	}

	// There was some error writing to the underlying connection
	// during the request, so don't re-use this conn.
	if w.conn.werr != nil {
		return false
	}

	if w.closedRequestBodyEarly() {
		return false
	}

	return true
}

// TrailerPrefix is a magic prefix for ResponseWriter.Header map keys
// that, if present, signals that the map entry is actually for
// the response trailers, and not the response headers. The prefix
// is stripped after the ServeHTTP call finishes and the values are
// sent in the trailers.
//
// This mechanism is intended only for trailers that are not known
// prior to the headers being written. If the set of trailers is fixed
// or known before the header is written, the normal Go trailers mechanism
// is preferred:
//
//	https://pkg.go.dev/net/http#ResponseWriter
//	https://pkg.go.dev/net/http#example-ResponseWriter-Trailers
const TrailerPrefix = "Trailer:"

// finalTrailers is called after the Handler exits and returns a non-nil
// value if the Handler set any trailers.
func (w *response) finalTrailers() http.Header {
	var t http.Header

	for k, vv := range w.handlerHeader {
		if kk, found := strings.CutPrefix(k, TrailerPrefix); found {
			if t == nil {
				t = make(http.Header)
			}

			t[kk] = vv
		}
	}

	for _, k := range w.trailers {
		if t == nil {
			t = make(http.Header)
		}

		for _, v := range w.handlerHeader[k] {
			t.Add(k, v)
		}
	}

	return t
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

func registerOnHitEOF(rc io.ReadCloser, fn func()) {
	switch v := rc.(type) {
	case *expectContinueReader:
		registerOnHitEOF(v.readCloser, fn)
	case *body:
		v.registerOnHitEOF(fn)
	default:
		panic("unexpected type " + fmt.Sprintf("%T", rc))
	}
}

// requestBodyRemains reports whether future calls to Read
// on rc might yield more data.
func requestBodyRemains(rc io.ReadCloser) bool {
	if rc == http.NoBody {
		return false
	}

	switch v := rc.(type) {
	case *expectContinueReader:
		return requestBodyRemains(v.readCloser)
	case *body:
		return v.bodyRemains()
	default:
		panic("unexpected type " + fmt.Sprintf("%T", rc))
	}
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
		tlsTO := c.server.tlsHandshakeTimeout()

		if tlsTO > 0 {
			dl := time.Now().Add(tlsTO)
			//nolint:errcheck
			c.rwc.SetReadDeadline(dl)
			//nolint:errcheck
			c.rwc.SetWriteDeadline(dl)
		}

		if err := tlsConn.HandshakeContext(ctx); err != nil {
			// If the handshake failed due to the client not speaking
			// TLS, assume they're speaking plaintext HTTP and write a
			// 400 response on the TLS conn's underlying net.Conn.
			var re tls.RecordHeaderError
			if errors.As(err, &re) && re.Conn != nil && tlsRecordHeaderLooksLikeHTTP(re.RecordHeader) {
				//nolint:errcheck
				io.WriteString(re.Conn, "HTTP/1.0 400 Bad Request\r\n\r\nClient sent an HTTP request to an HTTPS server.\n")
				//nolint:errcheck
				re.Conn.Close()

				return
			}

			c.server.logf("http: TLS handshake error from %s: %v", c.rwc.RemoteAddr(), err)

			return
		}

		// Restore Conn-level deadlines.
		if tlsTO > 0 {
			//nolint:errcheck
			c.rwc.SetReadDeadline(time.Time{})
			//nolint:errcheck
			c.rwc.SetWriteDeadline(time.Time{})
		}

		// Restore Conn-level deadlines.
		if tlsTO > 0 {
			//nolint:errcheck
			c.rwc.SetReadDeadline(time.Time{})
			//nolint:errcheck
			c.rwc.SetWriteDeadline(time.Time{})
		}

		c.tlsState = new(tls.ConnectionState)
		*c.tlsState = tlsConn.ConnectionState()

		if proto := c.tlsState.NegotiatedProtocol; validNextProto(proto) {
			if fn := c.server.TLSNextProto[proto]; fn != nil {
				h := initALPNRequest{ctx, tlsConn, serverHandler{c.server}}
				// Mark freshly created HTTP/2 as active and prevent any server state hooks
				// from being run on these connections. This prevents closeIdleConns from
				// closing such connections.
				c.setState(StateActive)
				fn(c.server, tlsConn, h)
			}

			return
		}
	}

	// HTTP/1.x from here on

	ctx, cancelCtx := context.WithCancel(ctx)
	c.cancelCtx = cancelCtx
	defer cancelCtx()

	c.r = &connReader{conn: c}
	c.bufr = newBufioReader(c.r)
	c.bufw = newBufioWriterSize(checkConnErrorWriter{c}, 4<<10)

	// each request from a single connection
	for {
		w, err := c.readRequest(ctx)

		if c.r.remain != c.server.initialReadLimitSize() {
			// If we read any bytes off the wire, we're active.
			c.setState(StateActive)
		}

		if err != nil {
			const errorHeaders = "\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\n\r\n"

			switch {
			case errors.Is(err, errTooLarge):
				// Their HTTP client may or may not be
				// able to read this if we're
				// responding to them and hanging up
				// while they're still writing their
				// request. Undefined behavior.
				const publicErr = "431 Request Header Fields Too Large"

				fmt.Fprintf(c.rwc, "HTTP/1.1 "+publicErr+errorHeaders+publicErr)

				c.closeWriteAndWait()

				return

			case isUnsupportedTEError(err):
				// Respond as per RFC 7230 Section 3.3.1 which says,
				//      A server that receives a request message with a
				//      transfer coding it does not understand SHOULD
				//      respond with 501 (Unimplemented).
				code := http.StatusNotImplemented

				// We purposefully aren't echoing back the transfer-encoding's value,
				// so as to mitigate the risk of cross side scripting by an attacker.
				fmt.Fprintf(
					c.rwc,
					"HTTP/1.1 %d %s%sUnsupported transfer encoding",
					code,
					http.StatusText(code),
					errorHeaders,
				)

				return

			case isCommonNetReadError(err):
				return // don't reply

			default:
				var v statusError

				if errors.As(err, &v) {
					fmt.Fprintf(c.rwc,
						"HTTP/1.1 %d %s: %s%s%d %s: %s",
						v.code,
						http.StatusText(v.code),
						v.text,
						errorHeaders,
						v.code,
						http.StatusText(v.code),
						v.text,
					)

					return
				}

				publicErr := "400 Bad Request"
				fmt.Fprintf(c.rwc, "HTTP/1.1 "+publicErr+errorHeaders+publicErr)

				return
			}
		}

		// Expect 100 Continue support
		req := w.req

		if ExpectsContinue(req) {
			if req.ProtoAtLeast(1, 1) && req.ContentLength != 0 {
				// Wrap the Body reader with one that replies on the connection
				req.Body = &expectContinueReader{readCloser: req.Body, resp: w}
				w.canWriteContinue.Store(true)
			}
		} else if GetHeader(req.Header, "Expect") != "" {
			w.sendExpectationFailed()
			return
		}

		c.curReq.Store(w)

		if requestBodyRemains(req.Body) {
			registerOnHitEOF(req.Body, w.conn.r.startBackgroundRead)
		} else {
			w.conn.r.startBackgroundRead()
		}

		// HTTP cannot have multiple simultaneous active requests.[*]
		// Until the server replies to this request, it can't read another,
		// so we might as well run the handler in this goroutine.
		// [*] Not strictly true: HTTP pipelining. We could let them all process
		// in parallel even if their responses need to be serialized.
		// But we're not going to implement HTTP pipelining because it
		// was never deployed in the wild and the answer is HTTP/2.
		inFlightResponse = w
		serverHandler{c.server}.ServeHTTP(w, w.req)

		inFlightResponse = nil

		w.cancelCtx()
		w.finishRequest()

		//nolint:errcheck
		c.rwc.SetWriteDeadline(time.Time{})

		if !w.shouldReuseConnection() {
			if w.requestBodyLimitHit || w.closedRequestBodyEarly() {
				c.closeWriteAndWait()
			}

			return
		}

		c.setState(StateIdle)
		c.curReq.Store(nil)

		if !w.conn.server.doKeepAlives() {
			// We're in shutdown mode. We might've replied
			// to the user without "Connection: close" and
			// they might think they can send another
			// request, but such is life with HTTP/1.1.
			return
		}

		if d := c.server.idleTimeout(); d != 0 {
			//nolint:errcheck
			c.rwc.SetReadDeadline(time.Now().Add(d))
		} else {
			//nolint:errcheck
			c.rwc.SetReadDeadline(time.Time{})
		}

		// Wait for the connection to become readable again before trying to
		// read the next request. This prevents a ReadHeaderTimeout or
		// ReadTimeout from starting until the first bytes of the next request
		// have been received.
		if _, err := c.bufr.Peek(4); err != nil {
			return
		}

		//nolint:errcheck
		c.rwc.SetReadDeadline(time.Time{})
	}
}

// http1ServerSupportsRequest reports whether Go's HTTP/1.x server
// supports the given request.
func http1ServerSupportsRequest(req *http.Request) bool {
	if req.ProtoMajor == 1 {
		return true
	}

	// Accept "PRI * HTTP/2.0" upgrade requests, so Handlers can
	// wire up their own HTTP/2 upgrades.
	if req.ProtoMajor == 2 && req.ProtoMinor == 0 &&
		req.Method == "PRI" && req.RequestURI == "*" {
		return true
	}

	// Reject HTTP/0.x, and all other HTTP/2+ requests (which
	// aren't encoded in ASCII anyway).
	return false
}

var textprotoReaderPool sync.Pool

func newTextprotoReader(br *bufio.Reader) *textproto.Reader {
	if v := textprotoReaderPool.Get(); v != nil {
		tr := v.(*textproto.Reader)
		tr.R = br

		return tr
	}

	return textproto.NewReader(br)
}

func putTextprotoReader(r *textproto.Reader) {
	r.R = nil

	textprotoReaderPool.Put(r)
}

// Read next request from connection.
// TODO readRequest is incomplete
func (c *conn) readRequest(ctx context.Context) (w *response, err error) {
	var (
		wholeReqDeadline time.Time // or zero if none
		hdrDeadline      time.Time // or zero if none
	)

	t0 := time.Now()

	if d := c.server.readHeaderTimeout(); d > 0 {
		hdrDeadline = t0.Add(d)
	}

	if d := c.server.ReadTimeout; d > 0 {
		wholeReqDeadline = t0.Add(d)
	}

	//nolint:errcheck
	c.rwc.SetReadDeadline(hdrDeadline)

	if d := c.server.WriteTimeout; d > 0 {
		defer func() {
			//nolint:errcheck
			c.rwc.SetWriteDeadline(time.Now().Add(d))
		}()
	}

	c.r.setReadLimit(c.server.initialReadLimitSize())

	if c.lastMethod == http.MethodPost {
		// RFC 7230 section 3 tolerance for old buggy clients.
		peek, _ := c.bufr.Peek(4) // ReadRequest will get err below
		//nolint:errcheck
		c.bufr.Discard(NumLeadingCRorLF(peek))
	}

	req, err := readRequest(c.bufr)
	if err != nil {
		if c.r.hitReadLimit() {
			return nil, errTooLarge
		}

		return nil, err
	}

	if !http1ServerSupportsRequest(req) {
		return nil, statusError{http.StatusHTTPVersionNotSupported, "unsupported protocol version"}
	}

	c.lastMethod = req.Method
	c.r.setInfiniteReadLimit()

	hosts, haveHost := req.Header["Host"]
	isH2Upgraded := isH2Upgrade(req)

	if req.ProtoAtLeast(1, 1) && (!haveHost || len(hosts) == 0) && !isH2Upgraded && req.Method != http.MethodConnect {
		return nil, badRequestError("missing required Host header")
	}

	if len(hosts) == 1 && !httpguts.ValidHostHeader(hosts[0]) {
		return nil, badRequestError("malformed Host header")
	}

	for k, vv := range req.Header {
		if !httpguts.ValidHeaderFieldName(k) {
			return nil, badRequestError("invalid header name")
		}

		for _, v := range vv {
			if !httpguts.ValidHeaderFieldValue(v) {
				return nil, badRequestError("invalid header value")
			}
		}
	}

	delete(req.Header, "Host")

	ctx, cancelCtx := context.WithCancel(ctx)

	// override context since ctx is a private variable
	// remove this line if it does not work
	req = req.WithContext(ctx)

	req.RemoteAddr = c.remoteAddr
	req.TLS = c.tlsState

	if body, ok := req.Body.(*body); ok {
		body.doEarlyClose = true
	}

	// Adjust the read deadline if necessary.
	if !hdrDeadline.Equal(wholeReqDeadline) {
		//nolint:errcheck
		c.rwc.SetReadDeadline(wholeReqDeadline)
	}

	w = &response{
		conn:          c,
		cancelCtx:     cancelCtx,
		req:           req,
		reqBody:       req.Body,
		handlerHeader: make(http.Header),
		contentLength: -1,
		closeNotifyCh: make(chan bool, 1),

		// We populate these ahead of time so we're not
		// reading from req.Header after their Handler starts
		// and maybe mutates it (Issue 14940)
		wants10KeepAlive: wantsHttp10KeepAlive(req),
		wantsClose:       wantsClose(req),
	}

	if isH2Upgraded {
		w.closeAfterReply = true
	}

	w.cw.res = w
	w.w = newBufioWriterSize(&w.cw, bufferBeforeChunkingSize)

	return w, nil
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

type closeWriter interface {
	CloseWrite() error
}

// closeWriteAndWait flushes any outstanding data and sends a FIN packet (if
// client is connected via TCP), signaling that we're done. We then
// pause for a bit, hoping the client processes it before any
// subsequent RST.
//
// See https://golang.org/issue/3595
func (c *conn) closeWriteAndWait() {
	c.finalFlush()

	if tcp, ok := c.rwc.(closeWriter); ok {
		//nolint:errcheck
		tcp.CloseWrite()
	}

	time.Sleep(rstAvoidanceDelay)
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

// errors

// badRequestError is a literal string (used by in the server in HTML,
// unescaped) to tell the user why their request was bad. It should
// be plain text without user info or other embedded errors.
func badRequestError(e string) error {
	return statusError{http.StatusBadRequest, e}
}

// statusError is an error used to respond to a request with an HTTP status.
// The text should be plain text without user info or other embedded errors.
type statusError struct {
	code int
	text string
}

func (e statusError) Error() string { return http.StatusText(e.code) + ": " + e.text }

// isCommonNetReadError reports whether err is a common error
// encountered during reading a request off the network when the
// client has gone away or had its read fail somehow. This is used to
// determine which logs are interesting enough to log about.
func isCommonNetReadError(err error) bool {
	if errors.Is(err, io.EOF) {
		return true
	}

	var neterr net.Error

	if errors.As(err, &neterr) && neterr.Timeout() {
		return true
	}

	var oe *net.OpError

	if errors.As(err, &oe) && oe.Op == "read" {
		return true
	}

	return false
}

// wrapper around io.ReadCloser which on first read, sends an
// HTTP/1.1 100 Continue header
type expectContinueReader struct {
	resp       *response
	readCloser io.ReadCloser
	closed     atomic.Bool
	sawEOF     atomic.Bool
}

func (ecr *expectContinueReader) Read(p []byte) (n int, err error) {
	if ecr.closed.Load() {
		return 0, http.ErrBodyReadAfterClose
	}

	w := ecr.resp

	if !w.wroteContinue && w.canWriteContinue.Load() {
		w.wroteContinue = true
		w.writeContinueMu.Lock()

		if w.canWriteContinue.Load() {
			//nolint:errcheck
			w.conn.bufw.WriteString("HTTP/1.1 100 Continue\r\n\r\n")
			//nolint:errcheck
			w.conn.bufw.Flush()
			w.canWriteContinue.Store(false)
		}

		w.writeContinueMu.Unlock()
	}

	n, err = ecr.readCloser.Read(p)

	if errors.Is(err, io.EOF) {
		ecr.sawEOF.Store(true)
	}

	return
}

func (ecr *expectContinueReader) Close() error {
	ecr.closed.Store(true)

	return ecr.readCloser.Close()
}

// extraHeader is the set of headers sometimes added by chunkWriter.writeHeader.
// This type is used to avoid extra allocations from cloning and/or populating
// the response Header map and all its 1-element slices.
type extraHeader struct {
	contentType      string
	connection       string
	transferEncoding string
	date             []byte // written if not nil
	contentLength    []byte // written if not nil
}

// extraHeader Methods
// Write writes the headers described in h to w.
//
// This method has a value receiver, despite the somewhat large size
// of h, because it prevents an allocation. The escape analysis isn't
// smart enough to realize this function doesn't mutate h.
func (h extraHeader) Write(w *bufio.Writer) {
	if h.date != nil {
		//nolint:errcheck
		w.Write(headerDate)
		//nolint:errcheck
		w.Write(h.date)
		//nolint:errcheck
		w.Write(crlf)
	}

	if h.contentLength != nil {
		//nolint:errcheck
		w.Write(headerContentLength)
		//nolint:errcheck
		w.Write(h.contentLength)
		//nolint:errcheck
		w.Write(crlf)
	}

	for i, v := range []string{h.contentType, h.connection, h.transferEncoding} {
		if v != "" {
			//nolint:errcheck
			w.Write(extraHeaderKeys[i])
			//nolint:errcheck
			w.Write(colonSpace)
			//nolint:errcheck
			w.WriteString(v)
			//nolint:errcheck
			w.Write(crlf)
		}
	}
}

// body turns a Reader into a ReadCloser.
// Close ensures that the body has been fully read
// and then reads the trailer if necessary.
type body struct {
	src          io.Reader
	hdr          any           // non-nil (Response or Request) value means read trailer
	r            *bufio.Reader // underlying wire-format reader for the trailer
	closing      bool          // is the connection to be closed after reading body?
	doEarlyClose bool          // whether Close should stop early

	mu         sync.Mutex // guards following, and calls to Read and Close
	sawEOF     bool
	closed     bool
	earlyClose bool   // Close called and we didn't read to the end of src
	onHitEOF   func() // if non-nil, func to call when EOF is Read
}

// ErrBodyReadAfterClose is returned when reading a Request or Response
// Body after the body has been closed. This typically happens when the body is
// read after an HTTP Handler calls WriteHeader or Write on its
// ResponseWriter.
var ErrBodyReadAfterClose = errors.New("http: invalid Read on closed Body")

func (b *body) Read(p []byte) (n int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return 0, ErrBodyReadAfterClose
	}

	return b.readLocked(p)
}

// Must hold b.mu.
func (b *body) readLocked(p []byte) (n int, err error) {
	if b.sawEOF {
		return 0, io.EOF
	}

	n, err = b.src.Read(p)

	if errors.Is(err, io.EOF) {
		b.sawEOF = true
		// Chunked case. Read the trailer.
		if b.hdr != nil {
			if e := b.readTrailer(); e != nil {
				err = e
				// Something went wrong in the trailer, we must not allow any
				// further reads of any kind to succeed from body, nor any
				// subsequent requests on the server connection. See
				// golang.org/issue/12027
				b.sawEOF = false
				b.closed = true
			}

			b.hdr = nil
		} else {
			// If the server declared the Content-Length, our body is a LimitedReader
			// and we need to check whether this EOF arrived early.
			if lr, ok := b.src.(*io.LimitedReader); ok && lr.N > 0 {
				err = io.ErrUnexpectedEOF
			}
		}
	}

	// If we can return an EOF here along with the read data, do
	// so. This is optional per the io.Reader contract, but doing
	// so helps the HTTP transport code recycle its connection
	// earlier (since it will see this EOF itself), even if the
	// client doesn't do future reads or Close.
	if err == nil && n > 0 {
		if lr, ok := b.src.(*io.LimitedReader); ok && lr.N == 0 {
			err = io.EOF
			b.sawEOF = true
		}
	}

	if b.sawEOF && b.onHitEOF != nil {
		b.onHitEOF()
	}

	return n, err
}

var (
	singleCRLF = []byte("\r\n")
	doubleCRLF = []byte("\r\n\r\n")
)

func seeUpcomingDoubleCRLF(r *bufio.Reader) bool {
	for peekSize := 4; ; peekSize++ {
		// This loop stops when Peek returns an error,
		// which it does when r's buffer has been filled.
		buf, err := r.Peek(peekSize)
		if bytes.HasSuffix(buf, doubleCRLF) {
			return true
		}

		if err != nil {
			break
		}
	}

	return false
}

var errTrailerEOF = errors.New("http: unexpected EOF reading trailer")

func (b *body) readTrailer() error {
	// The common case, since nobody uses trailers.
	buf, err := b.r.Peek(2)
	if bytes.Equal(buf, singleCRLF) {
		_, err = b.r.Discard(2)

		return err
	}

	if len(buf) < 2 {
		return errTrailerEOF
	}

	if err != nil {
		return err
	}

	// Make sure there's a header terminator coming up, to prevent
	// a DoS with an unbounded size Trailer. It's not easy to
	// slip in a LimitReader here, as textproto.NewReader requires
	// a concrete *bufio.Reader. Also, we can't get all the way
	// back up to our conn's LimitedReader that *might* be backing
	// this bufio.Reader. Instead, a hack: we iteratively Peek up
	// to the bufio.Reader's max size, looking for a double CRLF.
	// This limits the trailer to the underlying buffer size, typically 4kB.
	if !seeUpcomingDoubleCRLF(b.r) {
		return errors.New("http: suspiciously long trailer after chunked body")
	}

	hdr, err := textproto.NewReader(b.r).ReadMIMEHeader()

	if err != nil {
		if errors.Is(err, io.EOF) {
			return errTrailerEOF
		}

		return err
	}

	switch rr := b.hdr.(type) {
	case *http.Request:
		mergeSetHeader(&rr.Trailer, http.Header(hdr))
	case *http.Response:
		mergeSetHeader(&rr.Trailer, http.Header(hdr))
	}

	return nil
}

func mergeSetHeader(dst *http.Header, src http.Header) {
	if *dst == nil {
		*dst = src

		return
	}

	for k, vv := range src {
		(*dst)[k] = vv
	}
}

// unreadDataSizeLocked returns the number of bytes of unread input.
// It returns -1 if unknown.
// b.mu must be held.
func (b *body) unreadDataSizeLocked() int64 {
	if lr, ok := b.src.(*io.LimitedReader); ok {
		return lr.N
	}

	return -1
}

func (b *body) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil
	}

	var err error

	switch {
	case b.sawEOF:
		// Already saw EOF, so no need going to look for it.
	case b.hdr == nil && b.closing:
		// no trailer and closing the connection next.
		// no point in reading to EOF.
	case b.doEarlyClose:
		// Read up to maxPostHandlerReadBytes bytes of the body, looking
		// for EOF (and trailers), so we can re-use this connection.
		if lr, ok := b.src.(*io.LimitedReader); ok && lr.N > maxPostHandlerReadBytes {
			// There was a declared Content-Length, and we have more bytes remaining
			// than our maxPostHandlerReadBytes tolerance. So, give up.
			b.earlyClose = true
		} else {
			var n int64
			// Consume the body, or, which will also lead to us reading
			// the trailer headers after the body, if present.
			n, err = io.CopyN(io.Discard, bodyLocked{b}, maxPostHandlerReadBytes)

			if errors.Is(err, io.EOF) {
				err = nil
			}

			if n == maxPostHandlerReadBytes {
				b.earlyClose = true
			}
		}
	default:
		// Fully consume the body, which will also lead to us reading
		// the trailer headers after the body, if present.
		_, err = io.Copy(io.Discard, bodyLocked{b})
	}

	b.closed = true

	return err
}

func (b *body) didEarlyClose() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.earlyClose
}

// bodyRemains reports whether future Read calls might
// yield data.
func (b *body) bodyRemains() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	return !b.sawEOF
}

func (b *body) registerOnHitEOF(fn func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.onHitEOF = fn
}

// bodyLocked is an io.Reader reading from a *body when its mutex is
// already held.
type bodyLocked struct {
	b *body
}

func (bl bodyLocked) Read(p []byte) (n int, err error) {
	if bl.b.closed {
		return 0, ErrBodyReadAfterClose
	}

	return bl.b.readLocked(p)
}

// serverHandler delegates to either the server's Handler or
// DefaultServeMux and also handles "OPTIONS *" requests.
type serverHandler struct {
	srv *MiniServer
}

func (sh serverHandler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	handler := sh.srv.Handler

	if !sh.srv.DisableGeneralOptionsHandler && req.RequestURI == "*" && req.Method == http.MethodOptions {
		handler = globalOptionsHandler{}
	}

	handler.ServeHTTP(rw, req)
}

// globalOptionsHandler responds to "OPTIONS *" requests.
type globalOptionsHandler struct{}

func (globalOptionsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Length", "0")

	if r.ContentLength != 0 {
		// Read up to 4KB of OPTIONS body (as mentioned in the
		// spec as being reserved for future use), but anything
		// over that is considered a waste of server resources
		// (or an attack) and we abort and close the connection,
		// courtesy of MaxBytesReader's EOF behavior.
		mb := http.MaxBytesReader(w, r.Body, 4<<10)
		//nolint:errcheck
		io.Copy(io.Discard, mb)
	}
}

// initALPNRequest is an HTTP handler that initializes certain
// uninitialized fields in its *Request. Such partially-initialized
// Requests come from ALPN protocol handlers.
type initALPNRequest struct {
	//nolint:containedctx
	ctx context.Context
	c   *tls.Conn
	h   serverHandler
}

// BaseContext is an exported but unadvertised http.Handler method
// recognized by x/net/http2 to pass down a context; the TLSNextProto
// API predates context support so we shoehorn through the only
// interface we have available.
func (h initALPNRequest) BaseContext() context.Context { return h.ctx }

func (h initALPNRequest) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if req.TLS == nil {
		req.TLS = &tls.ConnectionState{}
		*req.TLS = h.c.ConnectionState()
	}

	if req.Body == nil {
		req.Body = http.NoBody
	}

	if req.RemoteAddr == "" {
		req.RemoteAddr = h.c.RemoteAddr().String()
	}

	h.h.ServeHTTP(rw, req)
}
