package web

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/ogefest/hoarfrost/internal/auth"
	"github.com/ogefest/hoarfrost/internal/version"
)

var funcs = template.FuncMap{
	"bytes":    humanBytes,
	"datetime": func(t time.Time) string { return fmtTime(t, "2006-01-02 15:04") },
	"date":     func(t time.Time) string { return fmtTime(t, "2006-01-02") },
	"ago":      ago,
	"pct":      func(f float64) string { return fmt.Sprintf("%.0f%%", f*100) },
	"can":      auth.Can,
	"version":  func() string { return version.Version },
	"icon": func(name string) template.HTML {
		return template.HTML(`<svg class="i" aria-hidden="true"><use href="/static/icons.svg#` + template.HTMLEscapeString(name) + `"></use></svg>`)
	},
	"under":    func(path, prefix string) bool { return path == prefix || strings.HasPrefix(path, prefix+"/") },
	"add":      func(a, b int) int { return a + b },
	"sub":      func(a, b int) int { return a - b },
	"truncate": truncate,
	"list":     func(v ...string) []string { return v },
	"dur":      humanDuration,
	"size":     formSize,
	"until":    until,
	"fields": func(f settingsForm, c formChoices, defaults bool) settingsFieldsData {
		return settingsFieldsData{F: f, C: c, Defaults: defaults}
	},
	"query": func(base string, kv ...any) string {
		v := url.Values{}
		for i := 0; i+1 < len(kv); i += 2 {
			if s := fmt.Sprint(kv[i+1]); s != "" && s != "0" {
				v.Set(fmt.Sprint(kv[i]), s)
			}
		}
		if len(v) == 0 {
			return base
		}
		return base + "?" + v.Encode()
	},
	"bar": func(n, max int64) int {
		if max == 0 {
			return 0
		}
		return int(n * 100 / max)
	},
}

func fmtTime(t time.Time, layout string) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format(layout)
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

// settingsFieldsData feeds the shared "settingsFields" template.
type settingsFieldsData struct {
	F        settingsForm
	C        formChoices
	Defaults bool
}

func until(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Until(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("in %d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("in %d h", int(d.Hours()))
	}
	return fmt.Sprintf("in %d days", int(d.Hours()/24))
}

func humanDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func parseTemplates() (map[string]*template.Template, error) {
	files, err := fs.Glob(assets, "templates/*.html")
	if err != nil {
		return nil, err
	}
	out := map[string]*template.Template{}
	for _, f := range files {
		name := path.Base(f)
		if name == "layout.html" || strings.HasPrefix(name, "_") {
			continue
		}
		t, err := template.New(name).Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/_*.html", f)
		if err != nil {
			return nil, fmt.Errorf("template %s: %w", name, err)
		}
		out[name] = t
	}
	return out, nil
}

// view is passed to every template.
type view struct {
	Title   string
	User    auth.User
	CSRF    string
	Path    string
	Message string
	Error   string
	Data    any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, page, title string, data any) {
	s.renderStatus(w, r, http.StatusOK, page, title, data)
}

func (s *Server) renderStatus(w http.ResponseWriter, r *http.Request, status int, page, title string, data any) {
	t, ok := s.pages[page]
	if !ok {
		http.Error(w, "unknown page "+page, http.StatusInternalServerError)
		return
	}
	v := view{Title: title, User: userFrom(r), Path: r.URL.Path, Message: r.URL.Query().Get("msg"), Error: r.URL.Query().Get("err"), Data: data}
	if sess := sessionFrom(r); sess != nil {
		v.CSRF = sess.CSRF
	}
	tmpl := "layout"
	if r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-Target") != "" {
		tmpl = "content"
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, tmpl, v); err != nil {
		s.Log.Error("render", "page", page, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

// fragment renders a named block without the layout (htmx partials).
func (s *Server) fragment(w http.ResponseWriter, r *http.Request, page, block string, data any) {
	v := view{User: userFrom(r), Data: data}
	if sess := sessionFrom(r); sess != nil {
		v.CSRF = sess.CSRF
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.pages[page].ExecuteTemplate(w, block, v); err != nil {
		s.Log.Error("render fragment", "page", page, "block", block, "err", err)
	}
}

func redirect(w http.ResponseWriter, r *http.Request, to, msg string) {
	if msg != "" {
		sep := "?"
		if strings.Contains(to, "?") {
			sep = "&"
		}
		to += sep + "msg=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func redirectErr(w http.ResponseWriter, r *http.Request, to string, err error) {
	sep := "?"
	if strings.Contains(to, "?") {
		sep = "&"
	}
	http.Redirect(w, r, to+sep+"err="+url.QueryEscape(err.Error()), http.StatusSeeOther)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func jsonError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func constantEqual(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func urlQueryEscape(s string) string { return url.QueryEscape(s) }

// safeNext accepts only local redirect targets.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}
