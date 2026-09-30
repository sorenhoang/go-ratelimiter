// Package httpx puts a limiter in front of an HTTP handler.
//
// It knows nothing about any particular algorithm: it holds a limiter.Limiter,
// asks it, and writes the answer. That is why five algorithms were added over
// five phases without this package changing once.
//
// Two decisions here are policy rather than mechanism, and both are config
// instead of being buried in code. What a request is limited *by* is a KeyFunc,
// so IP and API key are both reachable without touching the middleware. And
// what happens when the backend is unreachable is a bool, because there is no
// right answer to it — only a choice between losing protection and causing an
// outage with the thing meant to prevent one.
package httpx

import (
	"encoding/json"
	"net/http"
)

// ErrorBody is the JSON written for any refusal or failure.
//
// RetryAfter is in seconds and repeats the header of the same name, for clients
// that read a body more readily than a header. It is omitted when zero rather
// than sent as 0, which would read as "retry immediately".
type ErrorBody struct {
	Error      string `json:"error"`
	RetryAfter int64  `json:"retry_after,omitempty"`
}

// WriteJSON writes one JSON response.
//
// Call it at most once per request. A second call writes a second status line,
// which net/http drops with a "superfluous response.WriteHeader" in the log
// while the client keeps the first one.
//
// It exists because http.Error stamps Content-Type: text/plain over whatever
// body it is given, which had this server answering with JSON labelled as text
// for the whole of phase 00.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)

}
