package httpx

import (
	"net"
	"net/http"
)

// KeyFunc derives the rate limit key for a request.
//
// A KeyFunc always returns a non-empty string. Returning "" would drop every
// caller it cannot identify into one shared bucket, so a single client could
// exhaust the quota for all of them.
type KeyFunc func(r *http.Request) string

// KeyByIP returns the client IP with the port stripped, falling back to the raw
// RemoteAddr when that cannot be parsed.
//
// It deliberately ignores X-Forwarded-For. The client sets that header, so
// trusting it hands every caller a fresh key on every request and the limit
// stops applying at all. Behind a proxy you control, read XFF from the right and
// skip exactly the hops you own; everything further left is attacker-supplied.
// The opposite mistake costs just as much: behind a load balancer, without
// reading XFF, RemoteAddr is the balancer's own address and the entire service
// shares a single bucket.
func KeyByIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// KeyByHeader returns a KeyFunc reading the named header, falling back to
// KeyByIP when the header is absent so anonymous callers stay limited per IP
// instead of sharing one bucket.
func KeyByHeader(header string) KeyFunc {
	return func(r *http.Request) string {
		if v := r.Header.Get(header); v != "" {
			return v
		}
		return KeyByIP(r)
	}
}
