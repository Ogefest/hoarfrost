// Package sources discovers article URLs from RSS/Atom feeds, sitemaps and
// static URL lists.
package sources

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"

	"github.com/ogefest/hoarfrost/internal/config"
)

// Item is a discovered URL.
type Item struct {
	URL       string
	Title     string
	Published time.Time
}

// Fetcher downloads and parses feeds and sitemaps.
type Fetcher struct {
	Client      *http.Client
	UserAgent   string
	MaxBytes    int64
	MaxSitemaps int
	Now         func() time.Time
}

// NewFetcher returns a fetcher with sensible limits.
func NewFetcher(ua string) *Fetcher {
	return &Fetcher{Client: &http.Client{Timeout: time.Minute}, UserAgent: ua, MaxBytes: 50 << 20, MaxSitemaps: 50, Now: time.Now}
}

func (f *Fetcher) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if f.UserAgent != "" {
		req.Header.Set("User-Agent", f.UserAgent)
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sources: %s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, f.MaxBytes))
	if err != nil {
		return nil, err
	}
	if len(b) > 2 && b[0] == 0x1f && b[1] == 0x8b {
		gz, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		return io.ReadAll(io.LimitReader(gz, f.MaxBytes))
	}
	return b, nil
}

// Feed parses an RSS, Atom or JSON feed.
func (f *Fetcher) Feed(ctx context.Context, url string) ([]Item, error) {
	b, err := f.get(ctx, url)
	if err != nil {
		return nil, err
	}
	feed, err := gofeed.NewParser().Parse(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("sources: feed %s: %w", url, err)
	}
	var out []Item
	for _, it := range feed.Items {
		link := strings.TrimSpace(it.Link)
		if link == "" && len(it.Links) > 0 {
			link = strings.TrimSpace(it.Links[0])
		}
		if link == "" {
			continue
		}
		item := Item{URL: link, Title: strings.TrimSpace(it.Title)}
		if it.PublishedParsed != nil {
			item.Published = *it.PublishedParsed
		} else if it.UpdatedParsed != nil {
			item.Published = *it.UpdatedParsed
		}
		out = append(out, item)
	}
	return out, nil
}

type sitemapDoc struct {
	XMLName  xml.Name
	URLs     []sitemapURL `xml:"url"`
	Sitemaps []sitemapURL `xml:"sitemap"`
}

type sitemapURL struct {
	Loc     string      `xml:"loc"`
	LastMod string      `xml:"lastmod"`
	News    sitemapNews `xml:"news"`
}

type sitemapNews struct {
	Title           string `xml:"title"`
	PublicationDate string `xml:"publication_date"`
}

// Sitemap parses a sitemap or sitemap index (recursively, bounded).
func (f *Fetcher) Sitemap(ctx context.Context, url string) ([]Item, error) {
	var out []Item
	seen := map[string]bool{}
	var walk func(u string, depth int) error
	walk = func(u string, depth int) error {
		if seen[u] || len(seen) >= f.MaxSitemaps {
			return nil
		}
		seen[u] = true
		b, err := f.get(ctx, u)
		if err != nil {
			return err
		}
		var doc sitemapDoc
		if err := xml.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("sources: sitemap %s: %w", u, err)
		}
		for _, x := range doc.URLs {
			loc := strings.TrimSpace(x.Loc)
			if loc == "" {
				continue
			}
			item := Item{URL: loc, Title: strings.TrimSpace(x.News.Title)}
			item.Published = parseDate(x.News.PublicationDate)
			if item.Published.IsZero() {
				item.Published = parseDate(x.LastMod)
			}
			out = append(out, item)
		}
		if depth >= 3 {
			return nil
		}
		var errs []error
		for _, s := range doc.Sitemaps {
			if loc := strings.TrimSpace(s.Loc); loc != "" {
				if err := walk(loc, depth+1); err != nil {
					errs = append(errs, err)
				}
			}
		}
		return errors.Join(errs...)
	}
	err := walk(url, 0)
	return out, err
}

func parseDate(s string) time.Time {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04Z07:00", time.DateOnly} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// Discover lists URLs of a source. Errors of individual feeds do not stop
// the others. Dated items older than maxAge are skipped.
func (f *Fetcher) Discover(ctx context.Context, src config.Source, maxAge time.Duration) ([]Item, error) {
	var items []Item
	var errs []error
	for _, u := range src.Feeds {
		got, err := f.Feed(ctx, u)
		items = append(items, got...)
		if err != nil {
			errs = append(errs, err)
		}
	}
	for _, u := range src.Sitemaps {
		got, err := f.Sitemap(ctx, u)
		items = append(items, got...)
		if err != nil {
			errs = append(errs, err)
		}
	}
	for _, u := range src.URLs {
		items = append(items, Item{URL: u})
	}
	return filter(items, maxAge, f.Now()), errors.Join(errs...)
}

func filter(items []Item, maxAge time.Duration, now time.Time) []Item {
	seen := map[string]bool{}
	out := items[:0]
	for _, it := range items {
		if seen[it.URL] || config.ValidateURL(it.URL) != nil {
			continue
		}
		if maxAge > 0 && !it.Published.IsZero() && now.Sub(it.Published) > maxAge {
			continue
		}
		seen[it.URL] = true
		out = append(out, it)
	}
	return out
}
