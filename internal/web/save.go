package web

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/ogefest/hoarfrost/internal/coordinator"
)

// BookmarkletSource is the source of pages queued with the bookmarklet.
const BookmarkletSource = "bookmarklet"

type saveData struct {
	URL    string
	Title  string
	Queued bool
	Error  string
}

// savePage is opened by the bookmarklet with the page being viewed. With the
// user's bookmarklet key it queues the page at once; without it (an old
// bookmarklet, or a link from elsewhere) it asks for a confirmation, so no
// other website can make a signed-in browser queue URLs.
func (s *Server) savePage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d := saveData{URL: strings.TrimSpace(q.Get("url")), Title: strings.TrimSpace(q.Get("title"))}
	if d.URL == "" {
		d.Error = "No URL to save."
		s.renderStatus(w, r, http.StatusBadRequest, "save.html", "Save page", d)
		return
	}
	if k := q.Get("k"); k != "" {
		key, err := s.Auth.BookmarkletKey(r.Context(), userFrom(r).ID)
		if err == nil && constantEqual(k, key) {
			s.saveURL(w, r, d)
			return
		}
		d.Error = "This bookmarklet is no longer valid (the password was changed). Drag the new one from your account page."
	}
	s.render(w, r, "save.html", "Save page", d)
}

// save queues the URL confirmed on the save page.
func (s *Server) save(w http.ResponseWriter, r *http.Request) {
	s.saveURL(w, r, saveData{URL: strings.TrimSpace(r.PostFormValue("url")), Title: r.PostFormValue("title")})
}

func (s *Server) saveURL(w http.ResponseWriter, r *http.Request, d saveData) {
	if _, err := s.Coord.AddURL(r.Context(), coordinator.URLRequest{URL: d.URL, Source: BookmarkletSource}); err != nil {
		d.Error = err.Error()
		s.renderStatus(w, r, http.StatusBadRequest, "save.html", "Save page", d)
		return
	}
	d.Queued, d.Error = true, ""
	s.render(w, r, "save.html", "Saved", d)
}

// bookmarklet returns the javascript: URL that opens the save page for the
// page being viewed, in a small window (or the same tab when popups are
// blocked).
func bookmarklet(r *http.Request, key string) template.URL {
	base := url.URL{Scheme: "http", Host: r.Host, Path: "/save"}
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		base.Scheme = "https"
	}
	base.RawQuery = url.Values{"k": {key}}.Encode()
	prefix, _ := json.Marshal(base.String() + "&url=")
	js := `(function(){var u=` + string(prefix) +
		`+encodeURIComponent(location.href)+'&title='+encodeURIComponent(document.title);` +
		`if(!window.open(u,'hoarfrost','width=560,height=420'))location.href=u;})()`
	// The value is built here, never from user input.
	return template.URL("javascript:" + js)
}
