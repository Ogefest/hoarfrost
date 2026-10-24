// Package legacy imports an existing archive of single-file HTML pages
// (e.g. made with monolith) as WARC resource records, so the whole archive
// is searchable and readable in one format.
package legacy

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/ogefest/hoarfrost/internal/engine/static"
	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/extract"
	"github.com/ogefest/hoarfrost/internal/recorder"
)

// Source is the source name of imported pages.
const Source = "legacy"

// Recorder stores batches of records in an epoch.
type Recorder interface {
	Record(ctx context.Context, storage string, ep epoch.ID, source, engineName string, fn func(*recorder.Recorder) error) (recorder.CaptureFile, error)
}

// Options configure an import.
type Options struct {
	// Storage receives the imported pages ("" is the default storage).
	Storage string
	// BatchSize is the number of pages per capture file.
	BatchSize int
	// Date overrides file modification times.
	Date time.Time
	// Writable reports whether an epoch can still receive captures; pages of
	// sealed epochs go to the current epoch.
	Writable func(ctx context.Context, ep epoch.ID) bool
	Progress func(done, total int)
	Now      func() time.Time
}

// Report summarizes an import.
type Report struct {
	Pages  int
	Files  int
	Errors []string
}

type item struct {
	path string
	rel  string
	date time.Time
}

// Import walks dir for .html/.htm files and stores them grouped by epoch.
func Import(ctx context.Context, rec Recorder, dir string, o Options) (*Report, error) {
	if o.BatchSize <= 0 {
		o.BatchSize = 100
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	var items []item
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		ext := strings.ToLower(filepath.Ext(p))
		if ext != ".html" && ext != ".htm" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		date := o.Date
		if date.IsZero() {
			date = info.ModTime()
		}
		items = append(items, item{path: p, rel: filepath.ToSlash(rel), date: date.UTC()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].date.Before(items[j].date) || items[i].date.Equal(items[j].date) && items[i].rel < items[j].rel
	})

	byEpoch := map[epoch.ID][]item{}
	var order []epoch.ID
	for _, it := range items {
		ep := epoch.Of(it.date)
		if o.Writable != nil && !o.Writable(ctx, ep) {
			ep = epoch.Of(o.Now())
		}
		if _, ok := byEpoch[ep]; !ok {
			order = append(order, ep)
		}
		byEpoch[ep] = append(byEpoch[ep], it)
	}
	rep := &Report{}
	done := 0
	for _, ep := range order {
		batch := byEpoch[ep]
		for start := 0; start < len(batch); start += o.BatchSize {
			if err := ctx.Err(); err != nil {
				return rep, err
			}
			chunk := batch[start:min(start+o.BatchSize, len(batch))]
			_, err := rec.Record(ctx, o.Storage, ep, Source, "import", func(r *recorder.Recorder) error {
				for _, it := range chunk {
					if err := importFile(ctx, r, it); err != nil {
						rep.Errors = append(rep.Errors, fmt.Sprintf("%s: %v", it.rel, err))
						continue
					}
					rep.Pages++
				}
				return nil
			})
			if err != nil {
				return rep, err
			}
			rep.Files++
			done += len(chunk)
			if o.Progress != nil {
				o.Progress(done, len(items))
			}
		}
	}
	return rep, nil
}

func importFile(ctx context.Context, r *recorder.Recorder, it item) error {
	body, err := os.ReadFile(it.path)
	if err != nil {
		return err
	}
	uri := PageURL(body)
	if uri == "" {
		uri = "urn:legacy:" + it.rel
	}
	info, err := r.WriteResource(ctx, recorder.ResourceRecord{URI: uri, Date: it.date, ContentType: "text/html; charset=utf-8", Body: body})
	if err != nil {
		return err
	}
	ex := extract.HTML(body, "text/html", uri)
	p := recorder.PageInfo{
		URL: uri, RecordID: info.ID, Title: ex.Title, Text: ex.Text, Captured: it.date, Published: ex.Published,
		Source: Source, Engine: "import", Kind: "page", Mime: "text/html", Status: 200,
		Extra: map[string]string{"legacyPath": it.rel},
	}
	ex.Fill(&p)
	_, err = r.WriteMetadata(p)
	return err
}

// PageURL finds the original URL in <link rel=canonical> or og:url.
func PageURL(body []byte) string {
	doc, err := html.Parse(bytes.NewReader(body[:min(len(body), 256<<10)]))
	if err != nil {
		return ""
	}
	var canonical, og string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			attrs := map[string]string{}
			for _, a := range n.Attr {
				attrs[a.Key] = a.Val
			}
			switch n.DataAtom {
			case atom.Link:
				if strings.EqualFold(attrs["rel"], "canonical") && canonical == "" {
					canonical = attrs["href"]
				}
			case atom.Meta:
				if attrs["property"] == "og:url" && og == "" {
					og = attrs["content"]
				}
			case atom.Body:
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	for _, u := range []string{canonical, og} {
		if abs := static.Resolve(nil, u); abs != "" {
			return abs
		}
	}
	return ""
}
