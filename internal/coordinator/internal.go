package coordinator

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/worker"
)

// maxCompletionBytes bounds the size of a completion report.
const maxCompletionBytes = 256 << 20

// InternalHandler serves the coordinator-worker API protected by a token.
func (c *Coordinator) InternalHandler(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+worker.PathLease, func(w http.ResponseWriter, r *http.Request) {
		var req worker.LeaseRequest
		if !decode(w, r, &req, 1<<20) {
			return
		}
		jobs, err := c.Lease(r.Context(), req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, worker.LeaseResponse{Jobs: jobs})
	})
	mux.HandleFunc("POST "+worker.PathComplete, func(w http.ResponseWriter, r *http.Request) {
		var comp worker.Completion
		if !decode(w, r, &comp, maxCompletionBytes) {
			return
		}
		if err := c.Complete(r.Context(), comp); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, worker.ErrEpochClosed) {
				status = http.StatusConflict
			}
			http.Error(w, err.Error(), status)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST "+worker.PathDedup, func(w http.ResponseWriter, r *http.Request) {
		var req worker.DedupRequest
		if !decode(w, r, &req, 1<<16) {
			return
		}
		if _, err := epoch.Parse(string(req.Epoch)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ref, err := c.Lookup(r.Context(), req.Storage, req.Epoch, req.Digest)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, worker.DedupResponse{Ref: ref})
	})
	mux.HandleFunc("GET "+worker.PathBlocklist, func(w http.ResponseWriter, r *http.Request) {
		set, _ := c.Blocklist(r.Context())
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Blocklist-Version", set.Version())
		_ = set.Encode(w)
	})
	return RequireToken(token, mux)
}

// RequireToken rejects requests without "Authorization: Bearer <token>".
// An empty token disables the internal API entirely.
func RequireToken(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" || !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func decode(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit)).Decode(v); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
