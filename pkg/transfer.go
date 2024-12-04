package pkg

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"strconv"

	"golang.org/x/net/http/httpguts"
	"myHttpServer/internal"
)

// Determine whether to hang up after sending a request and body, or
// receiving a response and body
// 'header' is the request headers.
func shouldClose(major, minor int, header http.Header, removeCloseHeader bool) bool {
	if major < 1 {
		return true
	}

	conv := header["Connection"]
	hasClose := httpguts.HeaderValuesContainsToken(conv, "close")

	if major == 1 && minor == 0 {
		return hasClose || !httpguts.HeaderValuesContainsToken(conv, "keep-alive")
	}

	if hasClose && removeCloseHeader {
		header.Del("Connection")
	}

	return hasClose
}

// bodyAllowedForStatus reports whether a given response status code
// permits a body. See RFC 7230, section 3.3.
func bodyAllowedForStatus(status int) bool {
	switch {
	case status >= 100 && status <= 199:
		return false
	case status == 204:
		return false
	case status == 304:
		return false
	}

	return true
}

// parseContentLength trims whitespace from s and returns -1 if no value
// is set, or the value if it's >= 0.
func parseContentLength(cl string) (int64, error) {
	cl = textproto.TrimString(cl)
	if cl == "" {
		return -1, nil
	}

	n, err := strconv.ParseUint(cl, 10, 63)
	if err != nil {
		return 0, badStringError("bad Content-Length", cl)
	}

	return int64(n), nil
}

// Parse the trailer header.
func fixTrailer(header http.Header, chunked bool) (http.Header, error) {
	vv, ok := header["Trailer"]
	if !ok {
		return nil, nil
	}

	if !chunked {
		// Trailer and no chunking:
		// this is an invalid use case for trailer header.
		// Nevertheless, no error will be returned and we
		// let users decide if this is a valid HTTP message.
		// The Trailer header will be kept in Response.Header
		// but not populate Response.Trailer.
		// See issue #27197.
		return nil, nil
	}

	header.Del("Trailer")

	trailer := make(http.Header)

	var err error

	for _, v := range vv {
		foreachHeaderElement(v, func(key string) {
			key = http.CanonicalHeaderKey(key)
			switch key {
			case "Transfer-Encoding", "Trailer", "Content-Length":
				if err == nil {
					err = badStringError("bad trailer key", key)
					return
				}
			}

			trailer[key] = nil
		})
	}

	if err != nil {
		return nil, err
	}

	if len(trailer) == 0 {
		return nil, nil
	}

	return trailer, nil
}

func noResponseBodyExpected(requestMethod string) bool {
	return requestMethod == http.MethodHead
}

// Determine the expected body length, using RFC 7230 Section 3.3. This
// function is not a method, because ultimately it should be shared by
// ReadResponse and ReadRequest.
func fixLength(isResponse bool, status int, requestMethod string, header http.Header, chunked bool) (int64, error) {
	isRequest := !isResponse
	contentLens := header["Content-Length"]

	// Hardening against HTTP request smuggling
	if len(contentLens) > 1 {
		// Per RFC 7230 Section 3.3.2, prevent multiple
		// Content-Length headers if they differ in value.
		// If there are dups of the value, remove the dups.
		// See Issue 16490.
		first := textproto.TrimString(contentLens[0])
		for _, ct := range contentLens[1:] {
			if first != textproto.TrimString(ct) {
				return 0, fmt.Errorf(
					"http: message cannot contain multiple Content-Length headers; got %q",
					contentLens,
				)
			}
		}

		// deduplicate Content-Length
		header.Del("Content-Length")
		header.Add("Content-Length", first)

		contentLens = header["Content-Length"]
	}

	// Logic based on response type or status
	if isResponse && noResponseBodyExpected(requestMethod) {
		return 0, nil
	}

	if status/100 == 1 {
		return 0, nil
	}

	switch status {
	case 204, 304:
		return 0, nil
	}

	// Logic based on Transfer-Encoding
	if chunked {
		return -1, nil
	}

	// Logic based on Content-Length
	var cl string
	if len(contentLens) == 1 {
		cl = textproto.TrimString(contentLens[0])
	}

	if cl != "" {
		n, err := parseContentLength(cl)
		if err != nil {
			return -1, err
		}

		return n, nil
	}

	header.Del("Content-Length")

	if isRequest {
		// RFC 7230 neither explicitly permits nor forbids an
		// entity-body on a GET request so we permit one if
		// declared, but we default to 0 here (not -1 below)
		// if there's no mention of a body.
		// Likewise, all other request methods are assumed to have
		// no body if neither Transfer-Encoding chunked nor a
		// Content-Length are set.
		return 0, nil
	}

	// Body-EOF logic based on other methods (like closing, or chunked coding)
	return -1, nil
}

type transferReader struct {
	// Input
	Header        http.Header
	StatusCode    int
	RequestMethod string
	ProtoMajor    int
	ProtoMinor    int
	// Output
	Body          io.ReadCloser
	ContentLength int64
	Chunked       bool
	Close         bool
	Trailer       http.Header
}

func (t *transferReader) protoAtLeast(m, n int) bool {
	return t.ProtoMajor > m || (t.ProtoMajor == m && t.ProtoMinor >= n)
}

// parseTransferEncoding sets t.Chunked based on the Transfer-Encoding header.
func (t *transferReader) parseTransferEncoding() error {
	raw, present := t.Header["Transfer-Encoding"]
	if !present {
		return nil
	}

	delete(t.Header, "Transfer-Encoding")

	// Issue 12785; ignore Transfer-Encoding on HTTP/1.0 requests.
	if !t.protoAtLeast(1, 1) {
		return nil
	}

	// Like nginx, we only support a single Transfer-Encoding header field, and
	// only if set to "chunked". This is one of the most security sensitive
	// surfaces in HTTP/1.1 due to the risk of request smuggling, so we keep it
	// strict and simple.
	if len(raw) != 1 {
		return &unsupportedTEError{fmt.Sprintf("too many transfer encodings: %q", raw)}
	}

	if !internal.EqualFold(raw[0], "chunked") {
		return &unsupportedTEError{fmt.Sprintf("unsupported transfer encoding: %q", raw[0])}
	}

	// RFC 7230 3.3.2 says "A sender MUST NOT send a Content-Length header field
	// in any message that contains a Transfer-Encoding header field."
	//
	// but also: "If a message is received with both a Transfer-Encoding and a
	// Content-Length header field, the Transfer-Encoding overrides the
	// Content-Length. Such a message might indicate an attempt to perform
	// request smuggling (Section 9.5) or response splitting (Section 9.4) and
	// ought to be handled as an error. A sender MUST remove the received
	// Content-Length field prior to forwarding such a message downstream."
	//
	// Reportedly, these appear in the wild.
	delete(t.Header, "Content-Length")

	t.Chunked = true

	return nil
}

// msg is *Request or *Response.
func readTransfer(msg any, r *bufio.Reader) (err error) {
	t := &transferReader{RequestMethod: "GET"}

	// Unify input
	isResponse := false

	switch rr := msg.(type) {
	case *http.Response:
		t.Header = rr.Header
		t.StatusCode = rr.StatusCode
		t.ProtoMajor = rr.ProtoMajor
		t.ProtoMinor = rr.ProtoMinor
		t.Close = shouldClose(t.ProtoMajor, t.ProtoMinor, t.Header, true)
		isResponse = true

		if rr.Request != nil {
			t.RequestMethod = rr.Request.Method
		}
	case *http.Request:
		t.Header = rr.Header
		t.RequestMethod = rr.Method
		t.ProtoMajor = rr.ProtoMajor
		t.ProtoMinor = rr.ProtoMinor
		// Transfer semantics for Requests are exactly like those for
		// Responses with status code 200, responding to a GET method
		t.StatusCode = 200
		t.Close = rr.Close
	default:
		panic("unexpected type")
	}

	// Default to HTTP/1.1
	if t.ProtoMajor == 0 && t.ProtoMinor == 0 {
		t.ProtoMajor, t.ProtoMinor = 1, 1
	}

	// Transfer-Encoding: chunked, and overriding Content-Length.
	if err := t.parseTransferEncoding(); err != nil {
		return err
	}

	realLength, err := fixLength(isResponse, t.StatusCode, t.RequestMethod, t.Header, t.Chunked)
	if err != nil {
		return err
	}

	if isResponse && t.RequestMethod == http.MethodHead {
		if n, err := parseContentLength(GetHeader(t.Header, "Content-Length")); err != nil {
			return err
		} else {
			t.ContentLength = n
		}
	} else {
		t.ContentLength = realLength
	}

	// Trailer
	t.Trailer, err = fixTrailer(t.Header, t.Chunked)
	if err != nil {
		return err
	}

	// If there is no Content-Length or chunked Transfer-Encoding on a *Response
	// and the status is not 1xx, 204 or 304, then the body is unbounded.
	// See RFC 7230, section 3.3.
	switch msg.(type) {
	case *http.Response:
		if realLength == -1 && !t.Chunked && bodyAllowedForStatus(t.StatusCode) {
			// Unbounded body.
			t.Close = true
		}
	}

	// Prepare body reader. ContentLength < 0 means chunked encoding
	// or close connection when finished, since multipart is not supported yet
	switch {
	case t.Chunked:
		if isResponse && (noResponseBodyExpected(t.RequestMethod) || !bodyAllowedForStatus(t.StatusCode)) {
			t.Body = http.NoBody
		} else {
			t.Body = &body{src: internal.NewChunkedReader(r), hdr: msg, r: r, closing: t.Close}
		}
	case realLength == 0:
		t.Body = http.NoBody
	case realLength > 0:
		t.Body = &body{src: io.LimitReader(r, realLength), closing: t.Close}
	default:
		// realLength < 0, i.e. "Content-Length" not mentioned in header
		if t.Close {
			// Close semantics (i.e. HTTP/1.0)
			t.Body = &body{src: r, closing: t.Close}
		} else {
			// Persistent connection (i.e. HTTP/1.1)
			t.Body = http.NoBody
		}
	}

	// Unify output
	switch rr := msg.(type) {
	case *http.Request:
		rr.Body = t.Body
		rr.ContentLength = t.ContentLength

		if t.Chunked {
			rr.TransferEncoding = []string{"chunked"}
		}

		rr.Close = t.Close
		rr.Trailer = t.Trailer
	case *http.Response:
		rr.Body = t.Body
		rr.ContentLength = t.ContentLength

		if t.Chunked {
			rr.TransferEncoding = []string{"chunked"}
		}

		rr.Close = t.Close
		rr.Trailer = t.Trailer
	}

	return nil
}
