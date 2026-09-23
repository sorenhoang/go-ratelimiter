package httpx

import (
	"encoding/json"
	"net/http"
)

type ErrorBody struct {
	Error      string `json:"error"`
	RetryAfter int64  `json:"retry_after,omitempty"`
}

func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)

}
