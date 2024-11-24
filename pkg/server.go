package pkg

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net"
	"net/http"
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
)

type conn struct {
	// server is the server on which the connection arrived.
	// Immutable; never nil.
	server *MiniServer

	// rwc is the underlying network connection.
	// This is never wrapped by other types and is the value given out
	// to CloseNotifier callers. It is usually of type *net.TCPConn or
	// *tls.Conn.
	rwc net.Conn

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

func ListenAndServer(addr string, handler http.Handler) error {
	s := &MiniServer{Addr: addr, Handler: handler, SupportH2: false}

	return s.ListenAndServer()
}

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

func (c *conn) setState(state ConnState) {
	srv := c.server

	switch state {
	case StateNew:
		srv.trackConn(c, true)
	case StateClosed:
		srv.trackConn(c, false)
	}

	if state > 0xff || state < 0 {
		panic("internal error")
	}

	packedState := uint64(time.Now().Unix()<<8) | uint64(state)

	c.curState.Store(packedState)
}

func (c *conn) serve(ctx context.Context) {}
