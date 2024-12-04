package pkg

import (
	"net/http"

	"golang.org/x/net/http/httpguts"
)

// isProtocolSwitchHeader reports whether the request or response header
// is for a protocol switch.
func isProtocolSwitchHeader(h http.Header) bool {
	return h.Get("Upgrade") != "" &&
		httpguts.HeaderValuesContainsToken(h["Connection"], "Upgrade")
}

// isProtocolSwitchResponse reports whether the response code and
// response header indicate a successful protocol upgrade response.
func isProtocolSwitchResponse(code int, h http.Header) bool {
	return code == http.StatusSwitchingProtocols && isProtocolSwitchHeader(h)
}

func fixPragmaCacheControl(header http.Header) {
	if hp, ok := header["Pragma"]; ok && len(hp) > 0 && hp[0] == "no-cache" {
		if _, presentcc := header["Cache-Control"]; !presentcc {
			header["Cache-Control"] = []string{"no-cache"}
		}
	}
}
