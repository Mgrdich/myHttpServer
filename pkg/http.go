package pkg

import "time"

type contextKey struct {
	name string
}

func (k *contextKey) String() string {
	return "net/http context value " + k.name
}

// maxInt64 is the effective "infinite" value for the Server and
// Transport's byte-limiting readers.
const maxInt64 = 1<<63 - 1

// aLongTimeAgo is a non-zero time, far in the past, used for
// immediate cancellation of network operations.
var aLongTimeAgo = time.Unix(1, 0)
