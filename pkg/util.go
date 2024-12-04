package pkg

import (
	"fmt"
	"net/http"
	"strings"

	"golang.org/x/net/http/httpguts"
	"myHttpServer/internal"
)

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

// hasToken reports whether token appears with v, ASCII
// case-insensitive, with space or comma boundaries.
// token must be all lowercase.
// v may contain mixed cased.
func hasToken(v, token string) bool {
	if len(token) > len(v) || token == "" {
		return false
	}

	if v == token {
		return true
	}

	for sp := 0; sp <= len(v)-len(token); sp++ {
		// Check that first character is good.
		// The token is ASCII, so checking only a single byte
		// is sufficient. We skip this potential starting
		// position if both the first byte and its potential
		// ASCII uppercase equivalent (b|0x20) don't match.
		// False positives ('^' => '~') are caught by EqualFold.
		if b := v[sp]; b != token[0] && b|0x20 != token[0] {
			continue
		}

		// Check that start pos is on a valid token boundary.
		if sp > 0 && !isTokenBoundary(v[sp-1]) {
			continue
		}

		// Check that end pos is on a valid token boundary.
		if endPos := sp + len(token); endPos != len(v) && !isTokenBoundary(v[endPos]) {
			continue
		}

		if internal.EqualFold(v[sp:sp+len(token)], token) {
			return true
		}
	}

	return false
}

func isTokenBoundary(b byte) bool {
	return b == ' ' || b == ',' || b == '\t'
}

func ExpectsContinue(r *http.Request) bool {
	return hasToken(GetHeader(r.Header, "Expect"), "100-continue")
}

func NumLeadingCRorLF(v []byte) (n int) {
	for _, b := range v {
		if b == '\r' || b == '\n' {
			n++
			continue
		}

		break
	}

	return
}

func badStringError(what, val string) error {
	return fmt.Errorf("%s %q", what, val)
}

func isNotToken(r rune) bool {
	return !httpguts.IsTokenRune(r)
}

func ValidMethod(method string) bool {
	/*
	     Method         = "OPTIONS"                ; Section 9.2
	                    | "GET"                    ; Section 9.3
	                    | "HEAD"                   ; Section 9.4
	                    | "POST"                   ; Section 9.5
	                    | "PUT"                    ; Section 9.6
	                    | "DELETE"                 ; Section 9.7
	                    | "TRACE"                  ; Section 9.8
	                    | "CONNECT"                ; Section 9.9
	                    | extension-method
	   extension-method = token
	     token          = 1*<any CHAR except CTLs or separators>
	*/
	return len(method) > 0 && strings.IndexFunc(method, isNotToken) == -1
}

// isH2Upgrade reports whether r represents the http2 "client preface"
// magic string.
func isH2Upgrade(r *http.Request) bool {
	return r.Method == "PRI" && len(r.Header) == 0 && r.URL.Path == "*" && r.Proto == "HTTP/2.0"
}
