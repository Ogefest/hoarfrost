package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ogefest/hoarfrost/internal/coordinator"
	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/export"
	"github.com/ogefest/hoarfrost/internal/extract"
	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/queue"
	"github.com/ogefest/hoarfrost/internal/recorder"
	"github.com/ogefest/hoarfrost/internal/upload"
)

// ReplayWebVersion is the ReplayWeb.page release loaded from the CDN.
const ReplayWebVersion = "2.5.3"

type dashboardData struct {
	Stats      index.Stats
	MaxEpoch   int64
	MaxDay     int64
	Queue      map[string]int
	Failed     []queue.Job
	Workers    []coordinator.WorkerStatus
	Active     []string
	SealErrors map[string]string
	Ratio      float64
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st, err := s.idx.Stats(ctx, 30)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := dashboardData{Stats: st, Workers: s.Coord.Workers(), SealErrors: s.Coord.SealErrors(ctx)}
	if len(d.Stats.EpochSizes) > 12 {
		d.Stats.EpochSizes = d.Stats.EpochSizes[:12]
	}
	for _, e := range d.Stats.EpochSizes {
		d.MaxEpoch = max(d.MaxEpoch, e.Bytes)
	}
	for _, day := range st.PerDay {
		d.MaxDay = max(d.MaxDay, day.Pages)
	}
	if st.Bytes+st.DedupSaved > 0 {
		d.Ratio = float64(st.DedupSaved) / float64(st.Bytes+st.DedupSaved)
	}
	d.Queue, _ = s.queue.Counts(ctx)
	d.Failed, _ = s.queue.List(ctx, queue.Failed, 5, 0)
	if s.Active != nil {
		d.Active = s.Active()
	}
	s.render(w, r, "dashboard.html", "Dashboard", d)
}

type archiveData struct {
	Query    index.SearchQuery
	FromStr  string
	ToStr    string
	Results  []index.Page
	Total    int
	Sources  []string
	Storages []string
	Page     int
	Pages    int
}

const perPage = 50

func searchQuery(r *http.Request) (index.SearchQuery, int) {
	q := r.URL.Query()
	sq := index.SearchQuery{Q: q.Get("q"), Source: q.Get("source"), Storage: q.Get("storage"), Domain: q.Get("domain"), Kind: q.Get("kind"),
		Keyword: q.Get("keyword"), Limit: perPage}
	// Dates include the whole day; the API also takes RFC 3339 times.
	if t, err := time.ParseInLocation(time.DateOnly, q.Get("from"), time.Local); err == nil {
		sq.From = t
	} else if t, err := time.Parse(time.RFC3339, q.Get("from")); err == nil {
		sq.From = t
	}
	if t, err := time.ParseInLocation(time.DateOnly, q.Get("to"), time.Local); err == nil {
		sq.To = t.AddDate(0, 0, 1)
	} else if t, err := time.Parse(time.RFC3339, q.Get("to")); err == nil {
		sq.To = t
	}
	page, _ := strconv.Atoi(q.Get("page"))
	page = max(page, 1)
	sq.Offset = (page - 1) * perPage
	return sq, page
}

func (s *Server) archivePage(w http.ResponseWriter, r *http.Request) {
	sq, page := searchQuery(r)
	res, total, err := s.idx.Search(r.Context(), sq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	d := archiveData{Query: sq, FromStr: r.URL.Query().Get("from"), ToStr: r.URL.Query().Get("to"), Results: res, Total: total,
		Page: page, Pages: (total + perPage - 1) / perPage, Storages: s.Coord.Storages()}
	for _, src := range s.Coord.Sources().Sources {
		d.Sources = append(d.Sources, src.Name)
	}
	stats, _ := s.idx.SourceStats(r.Context())
	for _, ss := range stats {
		if _, ok := s.Coord.Sources().Find(ss.Source); !ok && ss.Source != "" {
			d.Sources = append(d.Sources, ss.Source)
		}
	}
	s.render(w, r, "archive.html", "Archive", d)
}

func (s *Server) pageByID(w http.ResponseWriter, r *http.Request) (index.Page, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return index.Page{}, false
	}
	p, err := s.idx.Page(r.Context(), id)
	if errors.Is(err, index.ErrNotFound) {
		http.NotFound(w, r)
		return p, false
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return p, false
	}
	return p, true
}

type pageData struct {
	Page     index.Page
	Meta     *recorder.PageInfo
	MetaErr  string
	Captures []index.Page
	Replay   bool
	Record   index.Record
	HTML     bool
	Data     structuredView
}

// structuredView is the structured data of a page prepared for display.
type structuredView struct {
	Types        []string // schema.org and microformats2 types
	OpenGraph    []nameValue
	SchemaOrg    string // indented JSON
	Microformats string
}

type nameValue struct{ Name, Value string }

func structured(p index.Page) structuredView {
	var v structuredView
	var og map[string]any
	if json.Unmarshal(p.OpenGraph, &og) == nil {
		names := make([]string, 0, len(og))
		for name := range og {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			for _, val := range extract.OpenGraphValues(og, name) {
				v.OpenGraph = append(v.OpenGraph, nameValue{name, val})
			}
		}
	}
	var mf []map[string]any
	_ = json.Unmarshal(p.Microformats, &mf)
	v.Types = append(append(v.Types, p.SchemaTypes...), extract.MicroformatTypes(mf)...)
	v.SchemaOrg, v.Microformats = indentJSON(p.SchemaOrg), indentJSON(p.Microformats)
	return v
}

func indentJSON(raw json.RawMessage) string {
	var b bytes.Buffer
	if len(raw) == 0 || string(raw) == "null" || json.Indent(&b, raw, "", "  ") != nil {
		return ""
	}
	return b.String()
}

func (s *Server) pageView(w http.ResponseWriter, r *http.Request) {
	p, ok := s.pageByID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	d := pageData{Page: p, Data: structured(p)}
	if meta, err := s.lib.PageMetadata(ctx, p); err != nil {
		d.MetaErr = err.Error()
	} else {
		d.Meta = meta
	}
	d.Captures, _ = s.idx.PageCaptures(ctx, p.URI)
	if info, err := s.idx.Epoch(ctx, p.Storage, p.Epoch); err == nil {
		d.Replay = info.State == index.StateSealed || info.State == index.StateVerified
	}
	if p.RecordID != "" {
		d.Record, _ = s.idx.Record(ctx, p.RecordID)
	}
	d.HTML = p.Mime == "" || p.Mime == "text/html" || p.Mime == upload.MHTMLContentType
	s.render(w, r, "page.html", p.Title, d)
}

// previewCSP sandboxes archived HTML: no scripts, no network access.
const previewCSP = "sandbox allow-popups allow-popups-to-escape-sandbox; default-src 'none'; img-src data:; style-src 'unsafe-inline' data:; font-src data:; media-src data:; frame-src data:"

// renderPage exports a page from the archive into one HTML document.
func (s *Server) renderPage(ctx context.Context, p index.Page, opts export.Options) ([]byte, error) {
	f := export.ArchiveFetcher{Library: s.lib, Storage: p.Storage, At: p.Captured}
	meta, err := s.lib.PageMetadata(ctx, p)
	if err == nil && meta.Extra["dom"] != "" {
		if rec, err := s.idx.Record(ctx, meta.Extra["dom"]); err == nil {
			if res, err := s.lib.Fetch(ctx, rec); err == nil {
				// The snapshot's relative references are those of the final URL.
				base := p.URI
				if final := meta.Extra["finalUrl"]; final != "" {
					base = final
				}
				return export.PageFromBody(ctx, f, res.Body, "text/html; charset=utf-8", base, opts)
			}
		}
	}
	if p.Mime == upload.MHTMLContentType && p.RecordID != "" {
		rec, err := s.idx.Record(ctx, p.RecordID)
		if err != nil {
			return nil, err
		}
		res, err := s.lib.Fetch(ctx, rec)
		if err != nil {
			return nil, err
		}
		m, err := upload.ParseMHTML(res.Body)
		if err != nil {
			return nil, err
		}
		h := m.HTML()
		if h == nil {
			return nil, errors.New("MHTML has no HTML part")
		}
		return export.PageFromBody(ctx, mhtmlFetcher{m, f}, h.Body, h.ContentType, p.URI, opts)
	}
	return export.Page(ctx, f, p.URI, opts)
}

type mhtmlFetcher struct {
	m        *upload.MHTML
	fallback export.Fetcher
}

func (f mhtmlFetcher) Fetch(ctx context.Context, url string) ([]byte, string, string, error) {
	for _, p := range f.m.Parts {
		if p.Location == url {
			return p.Body, p.ContentType, url, nil
		}
	}
	return f.fallback.Fetch(ctx, url)
}

func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	p, ok := s.pageByID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if p.Kind == "attachment" && p.RecordID != "" {
		rec, err := s.idx.Record(ctx, p.RecordID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		res, err := s.lib.Fetch(ctx, rec)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		ct := res.ContentType
		if strings.Contains(ct, "html") || strings.Contains(ct, "xml") || strings.Contains(ct, "svg") || ct == "" {
			w.Header().Set("Content-Security-Policy", previewCSP)
			ct = "text/plain; charset=utf-8"
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Content-Disposition", "inline")
		w.Write(res.Body)
		return
	}
	out, err := s.renderPage(ctx, p, export.Options{})
	if err != nil {
		http.Error(w, "preview unavailable: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Security-Policy", previewCSP)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(out)
}

type replayData struct {
	Page    index.Page
	Source  string
	TS      string
	Version string
}

func (s *Server) replayPage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.pageByID(w, r)
	if !ok {
		return
	}
	info, err := s.idx.Epoch(r.Context(), p.Storage, p.Epoch)
	if err != nil || (info.State != index.StateSealed && info.State != index.StateVerified) {
		http.Error(w, "full replay is available after the week is sealed; use the quick preview", http.StatusConflict)
		return
	}
	d := replayData{Page: p, Source: fmt.Sprintf("/wacz/%s/%04d/W%02d.wacz", index.StorageOr(p.Storage), p.Epoch.Year(), p.Epoch.Week()),
		TS: p.Captured.UTC().Format("20060102150405"), Version: ReplayWebVersion}
	s.render(w, r, "replay.html", "Replay: "+p.Title, d)
}

func (s *Server) replayWorker(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("Service-Worker-Allowed", "/replay/")
	fmt.Fprintf(w, "importScripts(\"https://cdn.jsdelivr.net/npm/replaywebpage@%s/sw.js\");\n", ReplayWebVersion)
}

// serveWACZ streams a weekly package with Range support.
func (s *Server) serveWACZ(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSuffix(r.PathValue("week"), ".wacz")
	ep, err := epoch.Parse(r.PathValue("year") + "-" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	arc, err := s.lib.Archive(r.PathValue("storage"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rd, err := arc.Storage.Open(r.Context(), ep.WeeklyKey())
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer rd.Close()
	modTime := time.Time{}
	if cat, err := arc.ReadCatalog(r.Context(), ep); err == nil {
		modTime = cat.SealedAt
	}
	w.Header().Set("Content-Type", "application/wacz+zip")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, ep.WeeklyKey(), modTime, io.NewSectionReader(rd, 0, rd.Size()))
}
