package web

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/ogefest/hoarfrost/internal/index"
)

// pageRef resolves a page by numeric ID or by capture ID (the UUID of its
// metadata record, with or without "urn:uuid:" and angle brackets).
func (s *Server) pageRef(r *http.Request, ref string) (index.Page, error) {
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		return s.idx.Page(r.Context(), id)
	}
	return s.idx.PageByMetadata(r.Context(), recordID(ref))
}

func (s *Server) apiPageByRef(w http.ResponseWriter, r *http.Request) (index.Page, bool) {
	p, err := s.pageRef(r, r.PathValue("id"))
	if errors.Is(err, index.ErrNotFound) {
		jsonError(w, http.StatusNotFound, errors.New("page not found"))
		return p, false
	}
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err)
		return p, false
	}
	return p, true
}

// apiPages lists pages in insertion order after a cursor, so that another
// application can follow the archive: it stores "next" and asks again.
func (s *Server) apiPages(w http.ResponseWriter, r *http.Request) {
	sq, _ := searchQuery(r)
	sq.Limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	var after int64
	if v := r.URL.Query().Get("after"); v != "" {
		var err error
		if after, err = strconv.ParseInt(v, 10, 64); err != nil || after < 0 {
			jsonError(w, http.StatusBadRequest, errors.New("after must be a page id"))
			return
		}
	}
	res, err := s.idx.PagesAfter(r.Context(), after, sq)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]apiPage, len(res))
	next := after
	for i, p := range res {
		out[i] = toAPI(p)
		next = p.ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": out, "next": next})
}

// apiPageContent returns the captured response body (HTML, PDF, image…) as
// it was archived, with its original content type.
func (s *Server) apiPageContent(w http.ResponseWriter, r *http.Request) {
	p, ok := s.apiPageByRef(w, r)
	if !ok {
		return
	}
	if p.RecordID == "" {
		jsonError(w, http.StatusNotFound, errors.New("this capture has no stored response"))
		return
	}
	ctx := r.Context()
	rec, err := s.idx.Record(ctx, p.RecordID)
	if err != nil {
		jsonError(w, http.StatusNotFound, err)
		return
	}
	res, err := s.lib.Fetch(ctx, rec)
	if err != nil {
		jsonError(w, http.StatusBadGateway, err)
		return
	}
	ct := res.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": contentFilename(p, ct)}))
	h.Set("Last-Modified", p.Captured.UTC().Format(http.TimeFormat))
	h.Set("X-Hoarfrost-URL", p.URI)
	h.Set("X-Hoarfrost-Captured", p.Captured.UTC().Format(time.RFC3339))
	if res.Status != 0 {
		h.Set("X-Hoarfrost-Status", strconv.Itoa(res.Status))
	}
	w.Write(res.Body)
}

// contentFilename names a downloaded capture after its URL path.
func contentFilename(p index.Page, ct string) string {
	name := path.Base(strings.SplitN(strings.SplitN(p.URI, "?", 2)[0], "#", 2)[0])
	if name == "" || name == "." || name == "/" || !strings.Contains(name, ".") {
		ext := ".bin"
		if mt, _, err := mime.ParseMediaType(ct); err == nil {
			switch mt {
			case "text/html":
				ext = ".html"
			case "application/pdf":
				ext = ".pdf"
			default:
				if exts, _ := mime.ExtensionsByType(mt); len(exts) > 0 {
					ext = exts[0]
				}
			}
		}
		name = fmt.Sprintf("capture-%s%s", captureID(p.MetadataID), ext)
	}
	return name
}

type apiFacet struct {
	Name     string    `json:"name"`
	Pages    int64     `json:"pages"`
	Captured time.Time `json:"lastCaptured"`
}

// apiFacets lists the values the search filters accept.
func (s *Server) apiFacets(w http.ResponseWriter, r *http.Request) {
	stats, err := s.idx.SourceStats(r.Context())
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err)
		return
	}
	sources := []apiFacet{}
	for _, st := range stats {
		if st.Source != "" {
			sources = append(sources, apiFacet{Name: st.Source, Pages: st.Pages, Captured: st.Last})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sources":  sources,
		"storages": s.Coord.Storages(),
		"kinds":    []string{"page", "attachment", "ocr"},
	})
}
