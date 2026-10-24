package blocklist

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Status describes the state of one list.
type Status struct {
	URL       string    `json:"url"`
	Entries   int       `json:"entries"`
	Fetched   time.Time `json:"fetched"`
	Checked   time.Time `json:"checked"`
	ETag      string    `json:"etag,omitempty"`
	Modified  string    `json:"lastModified,omitempty"`
	Error     string    `json:"error,omitempty"`
	FromCache bool      `json:"-"`
}

// Updater downloads lists periodically, keeps a local copy of each so the
// set survives restarts and network failures, and compiles them.
type Updater struct {
	URLs     []string
	Allow    []string
	CacheDir string
	Refresh  time.Duration
	Client   *http.Client
	Log      *slog.Logger
	// MaxBytes bounds a single list.
	MaxBytes int64
	Now      func() time.Time

	mu      sync.Mutex
	set     *Set
	status  map[string]*Status
	lists   map[string]Entries
	updated time.Time
}

// NewUpdater returns an updater; call Load and then Update periodically.
func NewUpdater(urls, allow []string, cacheDir string, refresh time.Duration, log *slog.Logger) *Updater {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if refresh <= 0 {
		refresh = 24 * time.Hour
	}
	return &Updater{URLs: urls, Allow: allow, CacheDir: cacheDir, Refresh: refresh, Log: log,
		Client: &http.Client{Timeout: 2 * time.Minute}, MaxBytes: 64 << 20, Now: time.Now,
		set: Empty(), status: map[string]*Status{}, lists: map[string]Entries{}}
}

// Set returns the current compiled set.
func (u *Updater) Set() *Set {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.set
}

// Statuses returns the state of each configured list.
func (u *Updater) Statuses() []Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]Status, 0, len(u.URLs))
	for _, url := range u.URLs {
		if st, ok := u.status[url]; ok {
			out = append(out, *st)
		} else {
			out = append(out, Status{URL: url})
		}
	}
	return out
}

func (u *Updater) key(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:12])
}

func (u *Updater) paths(url string) (data, meta string) {
	k := u.key(url)
	return filepath.Join(u.CacheDir, k+".txt"), filepath.Join(u.CacheDir, k+".json")
}

// Load compiles the set from cached copies without network access.
func (u *Updater) Load() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, url := range u.URLs {
		dataPath, metaPath := u.paths(url)
		st := &Status{URL: url}
		if b, err := os.ReadFile(metaPath); err == nil {
			_ = json.Unmarshal(b, st)
		}
		f, err := os.Open(dataPath)
		if err != nil {
			u.status[url] = st
			continue
		}
		e, err := Parse(f)
		f.Close()
		if err != nil {
			st.Error = err.Error()
		} else {
			u.lists[url] = e
			st.Entries = e.Len()
			st.FromCache = true
		}
		u.status[url] = st
	}
	u.compile()
	return nil
}

func (u *Updater) compile() {
	lists := make([]Entries, 0, len(u.URLs))
	for _, url := range u.URLs {
		if e, ok := u.lists[url]; ok {
			lists = append(lists, e)
		}
	}
	u.set = Compile(lists, u.Allow)
}

// Due reports whether the refresh interval has passed.
func (u *Updater) Due() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.URLs) > 0 && u.Now().Sub(u.updated) >= u.Refresh
}

// Update fetches every list (conditionally) and recompiles the set. A list
// that fails to download, or that suddenly parses to nothing although a
// previous copy had rules, keeps the previous copy.
func (u *Updater) Update(ctx context.Context) error {
	var errs []error
	for _, url := range u.URLs {
		if err := u.fetch(ctx, url); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", url, err))
			u.Log.Warn("blocklist update failed, keeping previous copy", "url", url, "err", err)
		}
	}
	u.mu.Lock()
	u.updated = u.Now()
	before := u.set.Version()
	u.compile()
	if u.set.Version() != before {
		u.Log.Info("blocklists updated", "rules", u.set.Len(), "version", u.set.Version())
	}
	u.mu.Unlock()
	return errors.Join(errs...)
}

func (u *Updater) fetch(ctx context.Context, url string) error {
	u.mu.Lock()
	st, ok := u.status[url]
	if !ok {
		st = &Status{URL: url}
		u.status[url] = st
	}
	prev := *st
	u.mu.Unlock()
	setErr := func(err error) error {
		u.mu.Lock()
		st.Checked, st.Error = u.Now(), err.Error()
		u.mu.Unlock()
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return setErr(err)
	}
	dataPath, metaPath := u.paths(url)
	if _, err := os.Stat(dataPath); err == nil {
		if prev.ETag != "" {
			req.Header.Set("If-None-Match", prev.ETag)
		}
		if prev.Modified != "" {
			req.Header.Set("If-Modified-Since", prev.Modified)
		}
	}
	resp, err := u.Client.Do(req)
	if err != nil {
		return setErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		u.mu.Lock()
		st.Checked, st.Error = u.Now(), ""
		u.mu.Unlock()
		return u.saveMeta(metaPath, st)
	}
	if resp.StatusCode != http.StatusOK {
		return setErr(fmt.Errorf("HTTP %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, u.MaxBytes+1))
	if err != nil {
		return setErr(err)
	}
	if int64(len(body)) > u.MaxBytes {
		return setErr(fmt.Errorf("list larger than %d bytes", u.MaxBytes))
	}
	e, err := Parse(bytes.NewReader(body))
	if err != nil {
		return setErr(err)
	}
	if e.Len() == 0 && prev.Entries > 0 {
		return setErr(errors.New("downloaded list has no rules, keeping previous copy"))
	}
	if err := os.MkdirAll(u.CacheDir, 0o755); err != nil {
		return setErr(err)
	}
	tmp := dataPath + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return setErr(err)
	}
	if err := os.Rename(tmp, dataPath); err != nil {
		return setErr(err)
	}
	u.mu.Lock()
	u.lists[url] = e
	now := u.Now()
	st.Entries, st.Fetched, st.Checked, st.Error, st.FromCache = e.Len(), now, now, "", false
	st.ETag, st.Modified = resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
	u.mu.Unlock()
	return u.saveMeta(metaPath, st)
}

func (u *Updater) saveMeta(path string, st *Status) error {
	u.mu.Lock()
	b, err := json.Marshal(st)
	u.mu.Unlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
