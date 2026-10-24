package static

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ogefest/hoarfrost/internal/engine"
)

// Default limits.
const (
	DefaultMaxResources = 300
	DefaultMaxCSSDepth  = 4
	DefaultConcurrency  = 6
)

// Engine captures a server-rendered page with all its resources.
type Engine struct{}

func (Engine) Name() string { return engine.Static }

type crawl struct {
	ctx      context.Context
	rec      engine.Recorder
	job      engine.Job
	opts     engine.Options
	sem      chan struct{}
	wg       sync.WaitGroup
	mu       sync.Mutex
	seen     map[string]bool
	fetched  int
	failed   int
	skipped  int
	links    []string
	linkSeen map[string]bool
}

func (Engine) Capture(ctx context.Context, job engine.Job, rec engine.Recorder) (engine.Result, error) {
	start := time.Now()
	opts := job.Options
	if opts.MaxResources == 0 {
		opts.MaxResources = DefaultMaxResources
	}
	if opts.MaxCSSDepth == 0 {
		opts.MaxCSSDepth = DefaultMaxCSSDepth
	}
	if opts.Concurrency == 0 {
		opts.Concurrency = DefaultConcurrency
	}
	resp, err := engine.Fetch(ctx, rec, job.URL, opts.UserAgent)
	if err != nil {
		return engine.Result{}, err
	}
	page := engine.Describe(ctx, job, engine.Static, resp)
	c := &crawl{ctx: ctx, rec: rec, job: job, opts: opts, sem: make(chan struct{}, opts.Concurrency),
		seen: map[string]bool{job.URL: true, resp.URL.String(): true}, linkSeen: map[string]bool{}}

	if page.Kind == "page" && resp.StatusCode < 400 {
		c.document(resp.Body, resp.Header.Get("Content-Type"), resp.URL, 0)
		c.wg.Wait()
	}

	page.Resources = c.fetched
	page.Failed = c.failed
	page.Blocked = c.skipped
	page.DurationMS = time.Since(start).Milliseconds()
	if _, err := rec.WriteMetadata(page); err != nil {
		return engine.Result{}, err
	}
	res := engine.Result{Page: page}
	if opts.Attachments {
		res.Discovered = c.links
	}
	if resp.StatusCode >= 400 {
		return res, &engine.StatusError{URL: job.URL, Status: resp.StatusCode}
	}
	res.NeedsFallback = page.Kind == "page" && opts.Fallback != "" && !job.Fallback && engine.TextShort(page.Text, opts)
	return res, nil
}

// document collects references of an HTML document (page or iframe).
func (c *crawl) document(body []byte, contentType string, base *url.URL, frameDepth int) {
	p, err := ParsePage(body, contentType, base, ParseOptions{SkipFrames: c.opts.SkipFrames})
	if err != nil {
		return
	}
	for _, l := range p.Links {
		c.mu.Lock()
		if !c.linkSeen[l] && !c.blocked(l) {
			c.linkSeen[l] = true
			c.links = append(c.links, l)
		}
		c.mu.Unlock()
	}
	for _, ic := range p.Inline {
		for _, r := range ResolveCSSRefs(ic.CSS, p.Base) {
			c.schedule(r, 1, frameDepth)
		}
	}
	for _, r := range p.Refs {
		if r.Kind == KindFrame && frameDepth >= 1 {
			continue
		}
		c.schedule(r, 0, frameDepth)
	}
}

func (c *crawl) blocked(raw string) bool {
	u, err := url.Parse(raw)
	return err != nil || c.job.IsBlocked(u.Hostname())
}

func (c *crawl) schedule(r Ref, cssDepth, frameDepth int) {
	c.mu.Lock()
	if c.seen[r.URL] || c.fetched+c.failed >= c.opts.MaxResources {
		c.mu.Unlock()
		return
	}
	if c.blocked(r.URL) {
		c.seen[r.URL] = true
		c.skipped++
		c.mu.Unlock()
		return
	}
	c.seen[r.URL] = true
	c.mu.Unlock()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		select {
		case c.sem <- struct{}{}:
		case <-c.ctx.Done():
			return
		}
		resp, err := engine.Fetch(c.ctx, c.rec, r.URL, c.opts.UserAgent)
		<-c.sem
		c.mu.Lock()
		if err != nil || resp.StatusCode >= 400 {
			c.failed++
		} else {
			c.fetched++
		}
		c.mu.Unlock()
		if err != nil || resp.StatusCode >= 400 {
			return
		}
		ct := resp.Header.Get("Content-Type")
		switch {
		case r.Kind == KindStyle || (r.Kind == KindAsset && strings.Contains(ct, "text/css")):
			if cssDepth < c.opts.MaxCSSDepth {
				for _, sub := range ResolveCSSRefs(resp.Body, resp.URL) {
					c.schedule(sub, cssDepth+1, frameDepth)
				}
			}
		case r.Kind == KindFrame && strings.Contains(ct, "html"):
			c.document(resp.Body, ct, resp.URL, frameDepth+1)
		}
	}()
}
