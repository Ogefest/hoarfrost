package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ogefest/hoarfrost/internal/coordinator"
	"github.com/ogefest/hoarfrost/internal/export"
	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/recorder"
	"github.com/ogefest/hoarfrost/internal/upload"
)

// MaxUpload bounds uploaded files.
const MaxUpload = 512 << 20

type apiPage struct {
	ID          int64           `json:"id"`
	CaptureID   string          `json:"captureId"`
	URL         string          `json:"url"`
	Title       string          `json:"title"`
	Description string          `json:"description,omitempty"`
	Captured    time.Time       `json:"captured"`
	Published   string          `json:"published,omitempty"`
	Authors     []string        `json:"authors,omitempty"`
	SchemaTypes []string        `json:"schemaTypes,omitempty"`
	SchemaOrg   json.RawMessage `json:"schemaOrg,omitempty"`
	OpenGraph   json.RawMessage `json:"openGraph,omitempty"`
	// Microformats is the JSON array of microformats2 items.
	Microformats json.RawMessage `json:"microformats,omitempty"`
	Keywords     []string        `json:"keywords,omitempty"`
	Source       string          `json:"source"`
	Engine       string          `json:"engine"`
	Kind         string          `json:"kind"`
	Mime         string          `json:"mime,omitempty"`
	Status       int             `json:"status,omitempty"`
	TextLength   int             `json:"textLength"`
	Storage      string          `json:"storage"`
	Epoch        string          `json:"epoch"`
	RecordID     string          `json:"recordId,omitempty"`
	Links        apiLinks        `json:"links"`
	Text         string          `json:"text,omitempty"`
}

type apiLinks struct {
	Self    string `json:"self"`
	Content string `json:"content,omitempty"`
	HTML    string `json:"html,omitempty"`
	Record  string `json:"record,omitempty"`
}

// rawJSON omits empty JSON values.
func rawJSON(v json.RawMessage) json.RawMessage {
	if len(v) == 0 || string(v) == "null" {
		return nil
	}
	return v
}

// captureID is the stable identifier of a capture: the UUID of its WARC
// metadata record. Page IDs change when an epoch is reindexed.
func captureID(metadataID string) string {
	return strings.TrimSuffix(strings.TrimPrefix(metadataID, "<urn:uuid:"), ">")
}

func toAPI(p index.Page) apiPage {
	out := apiPage{ID: p.ID, CaptureID: captureID(p.MetadataID), URL: p.URI, Title: p.Title, Description: p.Description,
		Captured: p.Captured, Published: p.Published, Authors: p.Authors, SchemaTypes: p.SchemaTypes, Source: p.Source,
		Engine: p.Engine, Kind: p.Kind, Mime: p.Mime, Status: p.Status, TextLength: p.TextLen, Storage: p.Storage,
		Epoch: string(p.Epoch), RecordID: p.RecordID}
	out.SchemaOrg, out.OpenGraph, out.Microformats, out.Keywords = rawJSON(p.SchemaOrg), rawJSON(p.OpenGraph), rawJSON(p.Microformats), p.Keywords
	out.Links.Self = "/api/pages/" + out.CaptureID
	if p.RecordID != "" {
		out.Links.Content = out.Links.Self + "/content"
		out.Links.Record = "/api/record/" + captureID(p.RecordID)
		if p.Kind != "attachment" {
			out.Links.HTML = "/api/export/" + out.CaptureID + ".html"
		}
	}
	return out
}

func (s *Server) apiSearch(w http.ResponseWriter, r *http.Request) {
	sq, _ := searchQuery(r)
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
		sq.Limit = l
	}
	if o, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil {
		sq.Offset = o
	}
	res, total, err := s.idx.Search(r.Context(), sq)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]apiPage, len(res))
	for i, p := range res {
		out[i] = toAPI(p)
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "results": out})
}

func (s *Server) apiPage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.apiPageByRef(w, r)
	if !ok {
		return
	}
	out := toAPI(p)
	if meta, err := s.lib.PageMetadata(r.Context(), p); err == nil {
		out.Text = meta.Text
	}
	writeJSON(w, http.StatusOK, out)
}

// recordID accepts "<urn:uuid:...>", "urn:uuid:..." or a bare UUID.
func recordID(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "<") {
		return raw
	}
	if !strings.HasPrefix(raw, "urn:") {
		raw = "urn:uuid:" + raw
	}
	return "<" + raw + ">"
}

func (s *Server) apiRecord(w http.ResponseWriter, r *http.Request) {
	rec, err := s.idx.Record(r.Context(), recordID(r.PathValue("id")))
	if errors.Is(err, index.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	raw, err := s.lib.RawRecord(r.Context(), rec)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	name := strings.Trim(strings.TrimPrefix(strings.Trim(rec.ID, "<>"), "urn:uuid:"), "/") + ".warc.gz"
	w.Header().Set("Content-Type", "application/warc")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Write(raw)
}

func (s *Server) apiExport(w http.ResponseWriter, r *http.Request) {
	p, err := s.pageRef(r, strings.TrimSuffix(r.PathValue("file"), ".html"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	out, err := s.renderPage(r.Context(), p, export.Options{
		Comment: fmt.Sprintf("Archived copy of %s captured %s (hoarfrost)", p.URI, p.Captured.UTC().Format(time.RFC3339)),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", previewCSP)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": fmt.Sprintf("page-%d.html", p.ID)}))
	w.Write(out)
}

type addURLRequest struct {
	URL     string `json:"url"`
	Source  string `json:"source"`
	Engine  string `json:"engine"`
	Storage string `json:"storage"`
}

func (s *Server) apiAddURL(w http.ResponseWriter, r *http.Request) {
	var req addURLRequest
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			jsonError(w, http.StatusBadRequest, err)
			return
		}
	} else {
		req = addURLRequest{URL: r.FormValue("url"), Source: r.FormValue("source"), Engine: r.FormValue("engine"), Storage: r.FormValue("storage")}
	}
	id, err := s.Coord.AddURL(r.Context(), coordinator.URLRequest{URL: req.URL, Source: req.Source, Engine: req.Engine, Storage: req.Storage})
	if err != nil {
		jsonError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"jobId": id, "url": req.URL, "storage": index.StorageOr(req.Storage)})
}

// apiUpload accepts a multipart form (any file field plus optional url,
// title and queue fields, as sent by SingleFile or the browser extension) or
// a raw body with the same fields as query parameters.
func (s *Server) apiUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxUpload)
	var u upload.Upload
	field := func(name string) string { return r.FormValue(name) }
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "multipart/form-data" {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			jsonError(w, http.StatusBadRequest, err)
			return
		}
		for _, files := range r.MultipartForm.File {
			if len(files) == 0 {
				continue
			}
			f, err := files[0].Open()
			if err != nil {
				jsonError(w, http.StatusBadRequest, err)
				return
			}
			u.Body, err = io.ReadAll(f)
			f.Close()
			if err != nil {
				jsonError(w, http.StatusBadRequest, err)
				return
			}
			u.Filename = files[0].Filename
			break
		}
	} else {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			jsonError(w, http.StatusBadRequest, err)
			return
		}
		u.Body = body
		u.Filename = r.URL.Query().Get("filename")
		field = r.URL.Query().Get
	}
	if len(u.Body) == 0 {
		jsonError(w, http.StatusBadRequest, errors.New("no file in request"))
		return
	}
	u.URL, u.Title = field("url"), field("title")
	if d := field("date"); d != "" {
		if t, err := time.Parse(time.RFC3339, d); err == nil {
			u.Date = t
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	defer cancel()
	var res *upload.Result
	storageName := field("storage")
	if storageName != "" && !s.lib.Has(storageName) {
		jsonError(w, http.StatusBadRequest, fmt.Errorf("unknown storage %q", storageName))
		return
	}
	cf, err := s.Coord.Record(ctx, storageName, "", "upload", "upload", func(rec *recorder.Recorder) error {
		var err error
		res, err = upload.Store(ctx, rec, u)
		return err
	})
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, upload.ErrUnknownFormat) || strings.HasPrefix(err.Error(), "upload:") {
			status = http.StatusBadRequest
		}
		jsonError(w, status, err)
		return
	}
	var queued []int64
	if q := field("queue"); q == "1" || q == "true" {
		for _, p := range res.Pages {
			if id, err := s.Coord.AddURL(ctx, coordinator.URLRequest{URL: p.URL, Source: "upload", Storage: storageName}); err == nil {
				queued = append(queued, id)
			}
		}
	}
	urls := make([]string, len(res.Pages))
	for i, p := range res.Pages {
		urls[i] = p.URL
	}
	writeJSON(w, http.StatusCreated, map[string]any{"kind": res.Kind, "pages": urls, "storage": cf.Storage, "capture": cf.Key(), "queued": queued})
}
