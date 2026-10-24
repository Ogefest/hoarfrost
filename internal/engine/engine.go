// Package engine defines how pages are captured. Engines differ only in how
// they fetch; everything is written through the shared recorder.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/extract"
	"github.com/ogefest/hoarfrost/internal/recorder"
)

// Engine names.
const (
	HTTP   = "http"
	Static = "static"
	Chrome = "chrome"
	Upload = "upload"
)

// Options are per-source capture settings delivered with a job.
type Options struct {
	Fallback        string        `json:"fallback,omitempty"`
	MinTextChars    int           `json:"minTextChars,omitempty"`
	Block           []string      `json:"block,omitempty"`
	SkipFrames      []string      `json:"skipFrames,omitempty"`
	MaxResponseSize int64         `json:"maxResponseSize,omitempty"`
	MaxResources    int           `json:"maxResources,omitempty"`
	MaxCSSDepth     int           `json:"maxCssDepth,omitempty"`
	Concurrency     int           `json:"concurrency,omitempty"`
	Attachments     bool          `json:"attachments,omitempty"`
	UserAgent       string        `json:"userAgent,omitempty"`
	ChromeScroll    bool          `json:"chromeScroll,omitempty"`
	ChromeWait      time.Duration `json:"chromeWait,omitempty"`
	Timeout         time.Duration `json:"timeout,omitempty"`
	// BlocklistVersion is the version of the global block lists the worker
	// must apply.
	BlocklistVersion string `json:"blocklistVersion,omitempty"`
}

// Job is one URL to capture.
type Job struct {
	ID      int64    `json:"id"`
	URL     string   `json:"url"`
	Source  string   `json:"source"`
	Storage string   `json:"storage,omitempty"`
	Engine  string   `json:"engine"`
	Epoch   epoch.ID `json:"epoch"`
	Options Options  `json:"options"`
	// Fallback marks a retry with the fallback engine.
	Fallback bool `json:"fallback,omitempty"`
	// Blocked reports hosts that must not be contacted (source block list
	// and global block lists); set by the worker.
	Blocked func(host string) bool `json:"-"`
}

// IsBlocked applies the job's block rules to a host.
func (j Job) IsBlocked(host string) bool {
	if j.Blocked != nil {
		return j.Blocked(host)
	}
	return hostMatches(host, j.Options.Block)
}

func hostMatches(host string, domains []string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, d := range domains {
		d = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(d)), ".")
		if d != "" && (host == d || strings.HasSuffix(host, "."+d)) {
			return true
		}
	}
	return false
}

// Result summarizes a capture.
type Result struct {
	Page recorder.PageInfo
	// NeedsFallback is set when the extracted text is shorter than the
	// source threshold and a fallback engine is configured.
	NeedsFallback bool
	// Discovered lists attachment URLs to capture with the http engine.
	Discovered []string
}

// Recorder is the part of the recorder engines use.
type Recorder interface {
	Client() *http.Client
	WriteResource(ctx context.Context, r recorder.ResourceRecord) (recorder.RecordInfo, error)
	WriteMetadata(p recorder.PageInfo) (recorder.RecordInfo, error)
}

// Engine captures one job into a recorder.
type Engine interface {
	Name() string
	Capture(ctx context.Context, job Job, rec Recorder) (Result, error)
}

// ErrUnknownEngine is returned for engines that are not registered.
var ErrUnknownEngine = errors.New("engine: unknown engine")

// Registry maps engine names to implementations.
type Registry map[string]Engine

// Get returns the named engine.
func (r Registry) Get(name string) (Engine, error) {
	if e, ok := r[name]; ok {
		return e, nil
	}
	return nil, fmt.Errorf("%w: %q", ErrUnknownEngine, name)
}

// Names lists registered engines.
func (r Registry) Names() []string {
	var out []string
	for n := range r {
		out = append(out, n)
	}
	return out
}

// StatusError reports a non-successful HTTP status of the main document.
type StatusError struct {
	URL    string
	Status int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("engine: %s returned HTTP %d", e.URL, e.Status)
}

// Response is a fetched document.
type Response struct {
	StatusCode int
	Header     http.Header
	// URL is the final URL after redirects.
	URL  *url.URL
	Body []byte
}

// Fetch performs a recorded GET and returns the response with its body.
func Fetch(ctx context.Context, rec Recorder, rawURL string, ua string) (*Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "pl,en;q=0.8")
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	resp, err := rec.Client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return &Response{StatusCode: resp.StatusCode, Header: resp.Header, URL: resp.Request.URL, Body: body}, nil
}

// Describe extracts text and fills page info for a captured document.
func Describe(ctx context.Context, job Job, engineName string, resp *Response) recorder.PageInfo {
	ct := resp.Header.Get("Content-Type")
	page := recorder.PageInfo{
		URL:      job.URL,
		RecordID: resp.Header.Get(recorder.HeaderRecordID),
		Source:   job.Source,
		Engine:   engineName,
		Fallback: job.Fallback,
		Status:   resp.StatusCode,
		Mime:     mediaType(ct),
		Kind:     "page",
	}
	if final := resp.URL.String(); final != job.URL {
		page.Extra = map[string]string{"finalUrl": final}
	}
	if !strings.HasPrefix(page.Mime, "text/html") && page.Mime != "application/xhtml+xml" && page.Mime != "" {
		page.Kind = "attachment"
	}
	if res, err := extract.Document(ctx, resp.Body, ct, job.URL); err == nil {
		page.Title = res.Title
		page.Text = res.Text
		page.Published = res.Published
		res.Structured.Fill(&page)
	}
	return page
}

// TextShort reports whether text is below the source's minimum length.
func TextShort(text string, opts Options) bool {
	return opts.MinTextChars > 0 && utf8.RuneCountInString(text) < opts.MinTextChars
}

func mediaType(ct string) string {
	mt, _, _ := strings.Cut(ct, ";")
	return strings.ToLower(strings.TrimSpace(mt))
}

// HTTPEngine fetches a single URL, typically a PDF or other attachment.
type HTTPEngine struct{}

func (HTTPEngine) Name() string { return HTTP }

func (HTTPEngine) Capture(ctx context.Context, job Job, rec Recorder) (Result, error) {
	start := time.Now()
	resp, err := Fetch(ctx, rec, job.URL, job.Options.UserAgent)
	if err != nil {
		return Result{}, err
	}
	page := Describe(ctx, job, HTTP, resp)
	page.DurationMS = time.Since(start).Milliseconds()
	if _, err := rec.WriteMetadata(page); err != nil {
		return Result{}, err
	}
	if resp.StatusCode >= 400 {
		return Result{Page: page}, &StatusError{URL: job.URL, Status: resp.StatusCode}
	}
	return Result{Page: page}, nil
}
