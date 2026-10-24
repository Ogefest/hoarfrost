// Package chrome captures pages that need JavaScript with headless Chromium.
// Every request of the browser is intercepted and performed by the
// recorder's client, so all traffic ends up in the WARC file exactly as
// with the other engines.
package chrome

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"

	"github.com/ogefest/hoarfrost/internal/engine"
	"github.com/ogefest/hoarfrost/internal/engine/static"
	"github.com/ogefest/hoarfrost/internal/extract"
	"github.com/ogefest/hoarfrost/internal/recorder"
)

// DOMPrefix marks resource records holding the rendered DOM of a page.
const DOMPrefix = "urn:dom:"

// Engine drives a shared headless browser; each capture gets its own
// incognito context.
type Engine struct {
	// Bin is the browser executable; empty means auto-detect.
	Bin string
	// NoSandbox is needed when running as root, e.g. in containers.
	NoSandbox bool
	// PageTimeout bounds a single capture.
	PageTimeout time.Duration

	mu      sync.Mutex
	browser *rod.Browser
}

// Available reports whether a browser binary can be found.
func Available(bin string) bool {
	if bin != "" {
		return true
	}
	_, ok := launcher.LookPath()
	return ok
}

func (*Engine) Name() string { return engine.Chrome }

func (e *Engine) connect() (*rod.Browser, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.browser != nil {
		return e.browser, nil
	}
	bin := e.Bin
	if bin == "" {
		var ok bool
		if bin, ok = launcher.LookPath(); !ok {
			return nil, errors.New("chrome: no browser found, set HOARFROST_CHROME")
		}
	}
	l := launcher.New().Bin(bin).Headless(true).Leakless(false).Set("disable-gpu").Set("mute-audio").
		Set("disable-background-networking").Set("disable-component-update")
	if e.NoSandbox {
		l = l.NoSandbox(true)
	}
	u, err := l.Launch()
	if err != nil {
		return nil, fmt.Errorf("chrome: launch: %w", err)
	}
	b := rod.New().ControlURL(u)
	if err := b.Connect(); err != nil {
		return nil, fmt.Errorf("chrome: connect: %w", err)
	}
	e.browser = b
	return b, nil
}

// Close shuts the browser down.
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.browser == nil {
		return nil
	}
	err := e.browser.Close()
	e.browser = nil
	return err
}

func (e *Engine) Capture(ctx context.Context, job engine.Job, rec engine.Recorder) (engine.Result, error) {
	start := time.Now()
	timeout := e.PageTimeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	b, err := e.connect()
	if err != nil {
		return engine.Result{}, err
	}
	incognito, err := b.Incognito()
	if err != nil {
		e.reset()
		return engine.Result{}, err
	}
	defer incognito.Close()
	page, err := incognito.Context(ctx).Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		return engine.Result{}, err
	}
	defer page.Close()

	client := rec.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var mu sync.Mutex
	var mainResp *http.Response
	resources, failed, blocked := 0, 0, 0
	router := page.HijackRequests()
	router.MustAdd("*", func(h *rod.Hijack) {
		u := h.Request.URL()
		if u.Scheme != "http" && u.Scheme != "https" {
			h.ContinueRequest(&proto.FetchContinueRequest{})
			return
		}
		if job.IsBlocked(u.Hostname()) {
			mu.Lock()
			blocked++
			mu.Unlock()
			h.Response.Fail(proto.NetworkErrorReasonBlockedByClient)
			return
		}
		if err := h.LoadResponse(client, true); err != nil {
			mu.Lock()
			failed++
			mu.Unlock()
			h.Response.Fail(proto.NetworkErrorReasonFailed)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		resp := h.Response.RawResponse
		isDoc := h.Request.Type() == proto.NetworkResourceTypeDocument
		if isDoc && mainResp == nil && (resp.StatusCode < 300 || resp.StatusCode >= 400) {
			mainResp = resp
		} else {
			resources++
		}
	})
	go router.Run()
	defer func() { _ = router.Stop() }()

	if err := page.Navigate(job.URL); err != nil {
		return engine.Result{}, fmt.Errorf("chrome: navigate: %w", err)
	}
	if err := page.WaitLoad(); err != nil {
		return engine.Result{}, fmt.Errorf("chrome: load: %w", err)
	}
	if job.Options.ChromeScroll {
		scroll(page)
	}
	wait := job.Options.ChromeWait
	if wait == 0 {
		wait = 2 * time.Second
	}
	page.Timeout(wait+5*time.Second).WaitRequestIdle(500*time.Millisecond, nil, nil, nil)()
	select {
	case <-ctx.Done():
		return engine.Result{}, ctx.Err()
	case <-time.After(wait):
	}
	dom, err := page.HTML()
	if err != nil {
		return engine.Result{}, fmt.Errorf("chrome: read DOM: %w", err)
	}

	mu.Lock()
	main := mainResp
	pageInfo := recorder.PageInfo{URL: job.URL, Source: job.Source, Engine: engine.Chrome, Fallback: job.Fallback,
		Kind: "page", Resources: resources, Failed: failed, Blocked: blocked}
	mu.Unlock()
	if main != nil {
		pageInfo.RecordID = main.Header.Get(recorder.HeaderRecordID)
		pageInfo.Status = main.StatusCode
		pageInfo.Mime = "text/html"
	}
	domInfo, err := rec.WriteResource(ctx, recorder.ResourceRecord{URI: DOMPrefix + job.URL, ContentType: "text/html; charset=utf-8",
		Body: []byte(dom), ConcurrentTo: pageInfo.RecordID})
	if err != nil {
		return engine.Result{}, err
	}
	pageInfo.Extra = map[string]string{"dom": domInfo.ID}
	if info, err := page.Info(); err == nil && info.URL != job.URL {
		pageInfo.Extra["finalUrl"] = info.URL
	}
	ex := extract.HTML([]byte(dom), "text/html; charset=utf-8", job.URL)
	pageInfo.Title, pageInfo.Text, pageInfo.Published = ex.Title, ex.Text, ex.Published
	ex.Structured.Fill(&pageInfo)
	pageInfo.DurationMS = time.Since(start).Milliseconds()
	if _, err := rec.WriteMetadata(pageInfo); err != nil {
		return engine.Result{}, err
	}
	res := engine.Result{Page: pageInfo}
	if main == nil {
		return res, fmt.Errorf("chrome: no document response for %s", job.URL)
	}
	if main.StatusCode >= 400 {
		return res, &engine.StatusError{URL: job.URL, Status: main.StatusCode}
	}
	if job.Options.Attachments {
		if p, err := static.ParsePage([]byte(dom), "text/html; charset=utf-8", main.Request.URL, static.ParseOptions{}); err == nil {
			res.Discovered = p.Links
		}
	}
	return res, nil
}

func (e *Engine) reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.browser != nil {
		e.browser.Close()
		e.browser = nil
	}
}

// scroll moves to the bottom of the page in steps to trigger lazy loading.
func scroll(page *rod.Page) {
	for range 20 {
		res, err := page.Eval(`() => { window.scrollBy(0, window.innerHeight); return window.scrollY + window.innerHeight >= document.body.scrollHeight }`)
		if err != nil || res.Value.Bool() {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	_, _ = page.Eval(`() => window.scrollTo(0, 0)`)
}
