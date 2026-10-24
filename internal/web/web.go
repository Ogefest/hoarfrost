// Package web serves the user interface (server-rendered html/template with
// htmx), the public API, WACZ files for replay and Prometheus metrics.
package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ogefest/hoarfrost/docs"
	"github.com/ogefest/hoarfrost/internal/archive"
	"github.com/ogefest/hoarfrost/internal/auth"
	"github.com/ogefest/hoarfrost/internal/coordinator"
	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/queue"
)

//go:embed all:templates static
var assets embed.FS

// SessionCookie is the name of the session cookie.
const SessionCookie = "hoarfrost_session"

// SessionTTL is the lifetime of a login session.
const SessionTTL = 14 * 24 * time.Hour

// Server is the HTTP front end.
type Server struct {
	Coord        *coordinator.Coordinator
	Auth         *auth.Store
	Log          *slog.Logger
	SecureCookie bool
	// WorkerToken enables the internal coordinator API when set.
	WorkerToken string
	// Active lists URLs being captured by the embedded worker.
	Active func() []string
	// Storage describes configured backends for the system page.
	Storage []string

	lib   *archive.Library
	idx   *index.DB
	queue *queue.Queue
	pages map[string]*template.Template

	limiter *limiter
	tasks   *tasks
}

// New prepares templates and helpers.
func New(s *Server) (*Server, error) {
	if s.Log == nil {
		s.Log = slog.Default()
	}
	s.lib = s.Coord.Library()
	s.idx = s.lib.Index
	s.queue = s.Coord.Queue()
	s.limiter = newLimiter(10, time.Minute)
	s.tasks = &tasks{}
	pages, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	s.pages = pages
	return s, nil
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(assets, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheForever(http.FileServerFS(static))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /api/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(docs.OpenAPI)
	})

	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /setup", s.setupPage)
	mux.HandleFunc("POST /setup", s.setup)

	viewer := func(p string, h http.HandlerFunc) { mux.Handle(p, s.require(auth.RoleViewer, h)) }
	editor := func(p string, h http.HandlerFunc) { mux.Handle(p, s.require(auth.RoleEditor, h)) }
	admin := func(p string, h http.HandlerFunc) { mux.Handle(p, s.require(auth.RoleAdmin, h)) }
	// reader routes also accept read-only API keys.
	reader := func(p string, h http.HandlerFunc) { mux.Handle(p, s.requireRead(h)) }

	viewer("GET /{$}", s.dashboard)
	viewer("GET /archive", s.archivePage)
	viewer("GET /page/{id}", s.pageView)
	viewer("GET /page/{id}/preview", s.preview)
	viewer("GET /replay/{id}", s.replayPage)
	viewer("GET /replay/sw.js", s.replayWorker)
	reader("GET /wacz/{storage}/{year}/{week}", s.serveWACZ)
	viewer("GET /account", s.accountPage)
	viewer("POST /account/tokens", s.createToken)
	viewer("POST /account/tokens/{id}/delete", s.deleteToken)
	viewer("POST /account/password", s.changePassword)
	viewer("GET /metrics", func(w http.ResponseWriter, r *http.Request) { s.Coord.Metrics.Handler().ServeHTTP(w, r) })

	editor("GET /sources", s.sourcesPage)
	editor("GET /sources/new", s.newSourcePage)
	editor("POST /sources/new", s.createSource)
	editor("POST /sources/test", s.testSource)
	editor("POST /sources/poll", s.pollSources)
	editor("GET /sources/{name}/edit", s.editSourcePage)
	editor("POST /sources/{name}/edit", s.updateSource)
	editor("POST /sources/{name}/delete", s.deleteSource)
	editor("POST /sources/{name}/test", s.testSource)
	editor("POST /sources/{name}/toggle", s.toggleSource)
	editor("POST /sources/{name}/poll", s.pollSource)
	editor("GET /sources/settings", s.settingsPage)
	editor("POST /sources/settings/defaults", s.saveDefaults)
	editor("POST /sources/settings/blocklists", s.saveBlocklistSettings)
	editor("POST /sources/settings/blocklists/update", s.updateBlocklists)
	editor("GET /queue", s.queuePage)
	editor("POST /queue/add", s.queueAdd)
	editor("POST /queue/{id}/retry", s.queueRetry)
	editor("POST /queue/retry-failed", s.queueRetryFailed)
	editor("POST /page/{id}/ocr", s.ocr)

	admin("GET /system", s.systemPage)
	admin("POST /system/epochs/{storage}/{epoch}/verify", s.verifyEpoch)
	admin("POST /system/epochs/{storage}/{epoch}/seal", s.sealEpoch)
	admin("POST /system/reindex", s.reindex)
	admin("POST /system/verify-all", s.verifyAll)
	admin("GET /system/tasks", s.taskStatus)
	admin("GET /system/keys", s.apiKeysPage)
	admin("POST /system/keys", s.createAPIKey)
	admin("POST /system/keys/{id}/delete", s.deleteAPIKey)
	admin("POST /system/users", s.createUser)
	admin("POST /system/users/{login}/delete", s.deleteUser)
	admin("POST /system/users/{login}/role", s.setRole)

	reader("GET /api/search", s.apiSearch)
	reader("GET /api/facets", s.apiFacets)
	reader("GET /api/pages", s.apiPages)
	reader("GET /api/pages/{id}", s.apiPage)
	reader("GET /api/pages/{id}/content", s.apiPageContent)
	reader("GET /api/record/{id}", s.apiRecord)
	reader("GET /api/export/{file}", s.apiExport)
	editor("POST /api/urls", s.apiAddURL)
	editor("POST /api/upload", s.apiUpload)
	editor("GET /api/sources", s.apiSources)
	editor("PUT /api/sources/{name}", s.apiPutSource)
	editor("DELETE /api/sources/{name}", s.apiDeleteSource)
	editor("POST /api/sources/{name}/poll", s.apiPollSource)
	editor("PUT /api/settings/defaults", s.apiPutDefaults)
	editor("PUT /api/settings/blocklists", s.apiPutBlocklists)
	editor("POST /api/blocklists/update", s.apiUpdateBlocklists)

	if s.WorkerToken != "" {
		mux.Handle("/internal/", s.Coord.InternalHandler(s.WorkerToken))
	}
	return s.recoverer(securityHeaders(mux))
}

func cacheForever(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		h.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		if !strings.HasSuffix(r.URL.Path, "/preview") && !strings.HasPrefix(r.URL.Path, "/replay/") && !strings.HasPrefix(r.URL.Path, "/wacz/") {
			h.Set("X-Frame-Options", "SAMEORIGIN")
			h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; frame-src 'self'; form-action 'self'; base-uri 'none'")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(v)
				}
				s.Log.Error("panic in handler", "path", r.URL.Path, "panic", v)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type ctxKey int

const (
	userKey ctxKey = iota
	sessionKey
)

func userFrom(r *http.Request) auth.User {
	u, _ := r.Context().Value(userKey).(auth.User)
	return u
}

func sessionFrom(r *http.Request) *auth.Session {
	s, _ := r.Context().Value(sessionKey).(*auth.Session)
	return s
}

// require authenticates by API token or session cookie and checks the role
// and, for cookie sessions, the CSRF token on state-changing requests.
func (s *Server) require(role string, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			u, err := s.Auth.TokenUser(ctx, tok)
			if err != nil {
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}
			if !auth.Can(u.Role, role) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			h(w, r.WithContext(context.WithValue(ctx, userKey, u)))
			return
		}
		c, err := r.Cookie(SessionCookie)
		var sess auth.Session
		if err == nil {
			sess, err = s.Auth.Session(ctx, c.Value)
		}
		if err != nil {
			if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/metrics" || r.Method != http.MethodGet {
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login?next="+urlQueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		if !auth.Can(sess.User.Role, role) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			tok := r.Header.Get("X-CSRF-Token")
			if tok == "" {
				tok = r.PostFormValue("csrf")
			}
			if !constantEqual(tok, sess.CSRF) {
				http.Error(w, "invalid CSRF token", http.StatusForbidden)
				return
			}
		}
		ctx = context.WithValue(ctx, userKey, sess.User)
		ctx = context.WithValue(ctx, sessionKey, &sess)
		h(w, r.WithContext(ctx))
	})
}

// requireRead accepts a read-only API key and otherwise falls back to
// require with the viewer role (session or user token).
func (s *Server) requireRead(h http.HandlerFunc) http.Handler {
	fallback := s.require(auth.RoleViewer, h)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !strings.HasPrefix(key, auth.APIKeyPrefix) {
			fallback.ServeHTTP(w, r)
			return
		}
		k, err := s.Auth.CheckAPIKey(r.Context(), key)
		if err != nil {
			http.Error(w, "invalid or expired API key", http.StatusUnauthorized)
			return
		}
		u := auth.User{Login: "apikey:" + k.Name, Role: auth.RoleViewer}
		h(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	})
}

// limiter allows a fixed number of attempts per key and window.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, hits: map[string][]time.Time{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	recent := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < l.window {
			recent = append(recent, t)
		}
	}
	if len(recent) >= l.max {
		l.hits[key] = recent
		return false
	}
	l.hits[key] = append(recent, now)
	return true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// tasks tracks long-running admin operations (reindex, verification).
type tasks struct {
	mu      sync.Mutex
	running string
	status  string
	last    string
	err     error
}

func (t *tasks) start(name string, fn func(progress func(string)) error) error {
	t.mu.Lock()
	if t.running != "" {
		t.mu.Unlock()
		return fmt.Errorf("%s is already running", t.running)
	}
	t.running, t.status, t.err = name, "starting", nil
	t.mu.Unlock()
	go func() {
		err := fn(func(s string) {
			t.mu.Lock()
			t.status = s
			t.mu.Unlock()
		})
		t.mu.Lock()
		t.last = fmt.Sprintf("%s finished at %s", t.running, time.Now().Format(time.DateTime))
		if err != nil {
			t.last = fmt.Sprintf("%s failed: %v", t.running, err)
		}
		t.running, t.err = "", err
		t.mu.Unlock()
	}()
	return nil
}

func (t *tasks) snapshot() (running, status, last string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running, t.status, t.last
}

// storageEpoch reads the {storage} and {epoch} path values.
func (s *Server) storageEpoch(r *http.Request) (*archive.Archive, epoch.ID, error) {
	arc, err := s.lib.Archive(r.PathValue("storage"))
	if err != nil {
		return nil, "", err
	}
	ep, err := epoch.Parse(r.PathValue("epoch"))
	return arc, ep, err
}
