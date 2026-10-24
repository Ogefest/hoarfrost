package web

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/ogefest/hoarfrost/internal/auth"
)

type apiKeysData struct {
	Keys   []auth.APIKey
	NewKey string
	Name   string
	Now    time.Time
}

func (s *Server) apiKeysPage(w http.ResponseWriter, r *http.Request) {
	s.renderAPIKeys(w, r, "", "")
}

func (s *Server) renderAPIKeys(w http.ResponseWriter, r *http.Request, newKey, name string) {
	keys, err := s.Auth.APIKeys(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, "apikeys.html", "API keys", apiKeysData{Keys: keys, NewKey: newKey, Name: name, Now: time.Now()})
}

func (s *Server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	var expires time.Time
	if v := r.PostFormValue("expires"); v != "" {
		t, err := time.ParseInLocation(time.DateOnly, v, time.Local)
		if err != nil {
			redirectErr(w, r, "/system/keys", errors.New("invalid expiry date"))
			return
		}
		// The key is valid through the whole chosen day.
		expires = t.AddDate(0, 0, 1).Add(-time.Second)
		if !expires.After(time.Now()) {
			redirectErr(w, r, "/system/keys", errors.New("the expiry date is in the past"))
			return
		}
	}
	name := r.PostFormValue("name")
	key, err := s.Auth.CreateAPIKey(r.Context(), name, userFrom(r).Login, expires)
	if err != nil {
		redirectErr(w, r, "/system/keys", err)
		return
	}
	s.renderAPIKeys(w, r, key, name)
}

func (s *Server) deleteAPIKey(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := s.Auth.DeleteAPIKey(r.Context(), id); err != nil {
		redirectErr(w, r, "/system/keys", err)
		return
	}
	redirect(w, r, "/system/keys", "API key revoked.")
}
