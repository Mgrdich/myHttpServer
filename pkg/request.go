package pkg

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// parseRequestLine parses "GET /foo HTTP/1.1" into its three parts.
func parseRequestLine(line string) (method, requestURI, proto string, ok bool) {
	method, rest, ok1 := strings.Cut(line, " ")
	requestURI, proto, ok2 := strings.Cut(rest, " ")

	if !ok1 || !ok2 {
		return "", "", "", false
	}

	return method, requestURI, proto, true
}

func readRequest(b *bufio.Reader) (req *http.Request, err error) {
	tp := newTextprotoReader(b)
	defer putTextprotoReader(tp)

	req = new(http.Request)

	var s string

	// First line: GET /index.html HTTP/1.0
	if s, err = tp.ReadLine(); err != nil {
		return nil, err
	}

	defer func() {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
	}()

	var ok bool
	req.Method, req.RequestURI, req.Proto, ok = parseRequestLine(s)

	if !ok {
		return nil, badStringError("malformed HTTP request", s)
	}

	if !ValidMethod(req.Method) {
		return nil, badStringError("invalid method", req.Method)
	}

	rawurl := req.RequestURI

	if req.ProtoMajor, req.ProtoMinor, ok = http.ParseHTTPVersion(req.Proto); !ok {
		return nil, badStringError("malformed HTTP version", req.Proto)
	}

	// CONNECT requests are used two different ways, and neither uses a full URL:
	// The standard use is to tunnel HTTPS through an HTTP proxy.
	// It looks like "CONNECT www.google.com:443 HTTP/1.1", and the parameter is
	// just the authority section of a URL. This information should go in req.URL.Host.
	//
	// The net/rpc package also uses CONNECT, but there the parameter is a path
	// that starts with a slash. It can be parsed with the regular URL parser,
	// and the path will end up in req.URL.Path, where it needs to be in order for
	// RPC to work.
	justAuthority := req.Method == http.MethodConnect && !strings.HasPrefix(rawurl, "/")
	if justAuthority {
		rawurl = "http://" + rawurl
	}

	if req.URL, err = url.ParseRequestURI(rawurl); err != nil {
		return nil, err
	}

	if justAuthority {
		// Strip the bogus "http://" back off.
		req.URL.Scheme = ""
	}

	// Subsequent lines: Key: value.
	mimeHeader, err := tp.ReadMIMEHeader()

	if err != nil {
		return nil, err
	}

	req.Header = http.Header(mimeHeader)
	if len(req.Header["Host"]) > 1 {
		return nil, fmt.Errorf("too many Host headers")
	}

	// RFC 7230, section 5.3: Must treat
	//	GET /index.html HTTP/1.1
	//	Host: www.google.com
	// and
	//	GET http://www.google.com/index.html HTTP/1.1
	//	Host: doesntmatter
	// the same. In the second case, any Host line is ignored.
	req.Host = req.URL.Host
	if req.Host == "" {
		req.Host = GetHeader(req.Header, "Host")
	}

	fixPragmaCacheControl(req.Header)

	req.Close = shouldClose(req.ProtoMajor, req.ProtoMinor, req.Header, false)

	err = readTransfer(req, b)

	if err != nil {
		return nil, err
	}

	if isH2Upgrade(req) {
		// Because it's neither chunked, nor declared:
		req.ContentLength = -1

		// We want to give handlers a chance to hijack the
		// connection, but we need to prevent the Server from
		// dealing with the connection further if it's not
		// hijacked. Set Close to ensure that:
		req.Close = true
	}

	return req, nil
}

// isH2Upgrade reports whether r represents the http2 "client preface"
// magic string.
func isH2Upgrade(r *http.Request) bool {
	return r.Method == "PRI" && len(r.Header) == 0 && r.URL.Path == "*" && r.Proto == "HTTP/2.0"
}

func ExpectsContinue(r *http.Request) bool {
	return hasToken(GetHeader(r.Header, "Expect"), "100-continue")
}

func GetHeader(h http.Header, key string) string {
	if v := h[key]; len(v) > 0 {
		return v[0]
	}

	return ""
}

func HasHeader(h http.Header, key string) bool {
	_, ok := h[key]

	return ok
}

func wantsHttp10KeepAlive(r *http.Request) bool {
	if r.ProtoMajor != 1 || r.ProtoMinor != 0 {
		return false
	}

	return hasToken(GetHeader(r.Header, "Connection"), "keep-alive")
}

func wantsClose(r *http.Request) bool {
	if r.Close {
		return true
	}

	return hasToken(GetHeader(r.Header, "Connection"), "close")
}
