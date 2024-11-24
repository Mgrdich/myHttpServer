package pkg

import (
	"crypto/tls"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type conn struct {
}

type MiniServer struct {
	// Addr optionally specifies the TCP address for the server to listen on,
	// in the form "host:port". If empty, ":http" (port 80) is used.
	Addr string

	Handler http.Handler // Should always be present unlike the go standard library

	// DisableGeneralOptionsHandler, if true, passes "OPTIONS *" requests to the Handler,
	// otherwise responds with 200 OK and Content-Length: 0.
	DisableGeneralOptionsHandler bool

	Certificate tls.Certificate

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
	// next request when keep-alives are enabled. If IdleTimeout
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

func (s *MiniServer) Serve(ln net.Listener) error {
	if s.shuttingDown() {
		return http.ErrServerClosed
	}

	return nil
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
