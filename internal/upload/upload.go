// Package upload turns pages saved in a browser (MHTML, SingleFile HTML,
// WACZ or WARC files) into archive records. Cookies and authorization
// headers are removed from everything that is stored.
package upload

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/mail"
	"net/textproto"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/ogefest/hoarfrost/internal/extract"
	"github.com/ogefest/hoarfrost/internal/recorder"
	"github.com/ogefest/hoarfrost/internal/warcio"
)

// Kinds of uploads.
const (
	KindMHTML      = "mhtml"
	KindSingleFile = "html"
	KindWACZ       = "wacz"
	KindWARC       = "warc"
)

// MHTMLContentType is used for MHTML resource records.
const MHTMLContentType = "multipart/related"

// Upload is a file received from a browser.
type Upload struct {
	Filename string
	Body     []byte
	// URL and Title override what is found in the file.
	URL   string
	Title string
	Date  time.Time
}

// ErrUnknownFormat is returned for unrecognized files.
var ErrUnknownFormat = errors.New("upload: unrecognized file format")

// Detect returns the kind of an upload.
func Detect(u Upload) (string, error) {
	name := strings.ToLower(u.Filename)
	head := u.Body[:min(len(u.Body), 4096)]
	switch {
	case bytes.HasPrefix(head, []byte("PK\x03\x04")):
		return KindWACZ, nil
	case bytes.HasPrefix(head, []byte{0x1f, 0x8b}), bytes.HasPrefix(head, []byte("WARC/")):
		return KindWARC, nil
	case strings.HasSuffix(name, ".mhtml"), strings.HasSuffix(name, ".mht"),
		bytes.Contains(bytes.ToLower(head), []byte("content-type: multipart/related")):
		return KindMHTML, nil
	case bytes.Contains(bytes.ToLower(head), []byte("<html")), bytes.Contains(bytes.ToLower(head), []byte("<!doctype html")),
		strings.HasSuffix(name, ".html"), strings.HasSuffix(name, ".htm"):
		return KindSingleFile, nil
	}
	return "", ErrUnknownFormat
}

// Writer is the part of the recorder used by uploads.
type Writer interface {
	WriteResource(ctx context.Context, r recorder.ResourceRecord) (recorder.RecordInfo, error)
	WriteMetadata(p recorder.PageInfo) (recorder.RecordInfo, error)
	RecordRaw(ctx context.Context, ex recorder.RawExchange) (recorder.Exchange, error)
}

// Result describes what was stored.
type Result struct {
	Kind  string
	Pages []recorder.PageInfo
}

// Store writes an upload through w.
func Store(ctx context.Context, w Writer, u Upload) (*Result, error) {
	kind, err := Detect(u)
	if err != nil {
		return nil, err
	}
	res := &Result{Kind: kind}
	switch kind {
	case KindMHTML:
		err = storeMHTML(ctx, w, u, res)
	case KindSingleFile:
		err = storeHTML(ctx, w, u, res)
	case KindWACZ:
		err = storeWACZ(ctx, w, u, res)
	case KindWARC:
		err = storeWARC(ctx, w, u.Body, u, res, nil)
	}
	return res, err
}

func page(u Upload, url, recordID, title, text, mimeType string) recorder.PageInfo {
	if u.Title != "" {
		title = u.Title
	}
	return recorder.PageInfo{URL: url, RecordID: recordID, Title: title, Text: text, Captured: u.Date,
		Engine: "upload", Kind: "page", Mime: mimeType, Status: 200}
}

var singleFileURL = regexp.MustCompile(`(?s)<!--\s*Page saved with SingleFile\s*url:\s*(\S+)`)

func orNow(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}

func storeHTML(ctx context.Context, w Writer, u Upload, res *Result) error {
	u.Date = orNow(u.Date)
	url := u.URL
	if url == "" {
		if m := singleFileURL.FindSubmatch(u.Body[:min(len(u.Body), 8192)]); m != nil {
			url = string(m[1])
		}
	}
	if url == "" {
		return errors.New("upload: page URL unknown, pass it explicitly")
	}
	info, err := w.WriteResource(ctx, recorder.ResourceRecord{URI: url, Date: u.Date, ContentType: "text/html; charset=utf-8", Body: u.Body})
	if err != nil {
		return err
	}
	ex := extract.HTML(u.Body, "text/html; charset=utf-8", url)
	p := page(u, url, info.ID, ex.Title, ex.Text, "text/html")
	p.Published = ex.Published
	ex.Fill(&p)
	if _, err := w.WriteMetadata(p); err != nil {
		return err
	}
	res.Pages = append(res.Pages, p)
	return nil
}

// MHTMLPart is one part of an MHTML archive.
type MHTMLPart struct {
	ContentType string
	Location    string
	Body        []byte
}

// MHTML is a parsed MHTML file.
type MHTML struct {
	URL   string
	Title string
	Date  time.Time
	Parts []MHTMLPart
}

// ParseMHTML parses an MHTML (multipart/related) document.
func ParseMHTML(b []byte) (*MHTML, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("upload: mhtml: %w", err)
	}
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mt, "multipart/") {
		return nil, fmt.Errorf("upload: mhtml: not a multipart document")
	}
	m := &MHTML{URL: msg.Header.Get("Snapshot-Content-Location")}
	if dec, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject")); err == nil {
		m.Title = dec
	}
	m.Date, _ = msg.Header.Date()
	mr := multipart.NewReader(msg.Body, params["boundary"])
	for {
		p, err := mr.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("upload: mhtml: %w", err)
		}
		body, err := io.ReadAll(decodeTransfer(p.Header, p))
		if err != nil {
			return nil, fmt.Errorf("upload: mhtml part: %w", err)
		}
		m.Parts = append(m.Parts, MHTMLPart{ContentType: p.Header.Get("Content-Type"), Location: p.Header.Get("Content-Location"), Body: body})
	}
	if m.URL == "" && len(m.Parts) > 0 {
		m.URL = m.Parts[0].Location
	}
	return m, nil
}

func decodeTransfer(h textproto.MIMEHeader, r io.Reader) io.Reader {
	switch strings.ToLower(strings.TrimSpace(h.Get("Content-Transfer-Encoding"))) {
	case "base64":
		return base64Reader(r)
	case "quoted-printable":
		return qpReader(r)
	}
	return r
}

// HTML returns the main HTML part.
func (m *MHTML) HTML() *MHTMLPart {
	for i := range m.Parts {
		if strings.HasPrefix(m.Parts[i].ContentType, "text/html") {
			return &m.Parts[i]
		}
	}
	return nil
}

func storeMHTML(ctx context.Context, w Writer, u Upload, res *Result) error {
	m, err := ParseMHTML(u.Body)
	if err != nil {
		return err
	}
	url := u.URL
	if url == "" {
		url = m.URL
	}
	if url == "" {
		return errors.New("upload: page URL unknown, pass it explicitly")
	}
	if u.Date.IsZero() {
		u.Date = m.Date
	}
	u.Date = orNow(u.Date)
	info, err := w.WriteResource(ctx, recorder.ResourceRecord{URI: url, Date: u.Date, ContentType: MHTMLContentType, Body: u.Body})
	if err != nil {
		return err
	}
	var ex extract.Result
	if h := m.HTML(); h != nil {
		ex = extract.HTML(h.Body, h.ContentType, url)
	}
	title := ex.Title
	if title == "" {
		title = m.Title
	}
	p := page(u, url, info.ID, title, ex.Text, MHTMLContentType)
	p.Published = ex.Published
	ex.Fill(&p)
	if _, err := w.WriteMetadata(p); err != nil {
		return err
	}
	res.Pages = append(res.Pages, p)
	return nil
}

func storeWACZ(ctx context.Context, w Writer, u Upload, res *Result) error {
	zr, err := zip.NewReader(bytes.NewReader(u.Body), int64(len(u.Body)))
	if err != nil {
		return fmt.Errorf("upload: wacz: %w", err)
	}
	titles := map[string]string{}
	if f, err := zr.Open("pages/pages.jsonl"); err == nil {
		titles = readPageTitles(f)
		f.Close()
	}
	found := false
	for _, f := range zr.File {
		isWARC := strings.HasSuffix(f.Name, ".warc.gz") || strings.HasSuffix(f.Name, ".warc")
		if !strings.HasPrefix(f.Name, "archive/") || !isWARC {
			continue
		}
		found = true
		rc, err := f.Open()
		if err != nil {
			return err
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}
		if err := storeWARC(ctx, w, data, u, res, titles); err != nil {
			return fmt.Errorf("upload: %s: %w", path.Base(f.Name), err)
		}
	}
	if !found {
		return errors.New("upload: wacz contains no WARC files")
	}
	return nil
}

// storeWARC re-records HTTP exchanges and resources of a foreign WARC file.
// Pages are the URLs listed in titles (from pages.jsonl) or, without a list,
// the first HTML response.
func storeWARC(ctx context.Context, w Writer, data []byte, u Upload, res *Result, titles map[string]string) error {
	u.Date = orNow(u.Date)
	recs, err := warcio.ReadAll(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("upload: warc: %w", err)
	}
	requests := map[string][]byte{}
	for _, r := range recs {
		if r.Type() == warcio.TypeRequest {
			if id := r.Header.Get(warcio.HConcurrentTo); id != "" {
				requests[id] = r.Block
			}
		}
	}
	type candidate struct {
		url, id, mime string
		body          []byte
		ct            string
	}
	var htmlPages []candidate
	for _, r := range recs {
		date, _ := r.Date()
		uri := r.TargetURI()
		switch r.Type() {
		case warcio.TypeResponse:
			if !strings.HasPrefix(r.Header.Get(warcio.HContentType), "application/http") {
				continue
			}
			ex, err := w.RecordRaw(ctx, recorder.RawExchange{
				URI: uri, Date: date, IP: r.Header.Get(warcio.HIPAddress), Request: requests[r.ID()], Response: r.Block,
				Truncated: r.Header.Get(warcio.HTruncated) != "", StripResponse: recorder.SensitiveResponseHeaders,
			})
			if err != nil {
				return err
			}
			if resp, err := warcio.ParseHTTPResponse(r.Block); err == nil && resp.StatusCode == http.StatusOK {
				ct := resp.Header.Get("Content-Type")
				if strings.HasPrefix(ct, "text/html") {
					body, _ := recorder.Decode(resp.Header.Get("Content-Encoding"), resp.Payload)
					htmlPages = append(htmlPages, candidate{uri, ex.RecordID, "text/html", body, ct})
				}
			}
		case warcio.TypeResource:
			info, err := w.WriteResource(ctx, recorder.ResourceRecord{URI: uri, Date: date, ContentType: r.Header.Get(warcio.HContentType), Body: r.Block})
			if err != nil {
				return err
			}
			if strings.HasPrefix(r.Header.Get(warcio.HContentType), "text/html") {
				htmlPages = append(htmlPages, candidate{uri, info.ID, "text/html", r.Block, r.Header.Get(warcio.HContentType)})
			}
		}
	}
	for i, c := range htmlPages {
		title, listed := titles[c.url]
		switch {
		case u.URL != "" && c.url != u.URL:
			continue
		case u.URL == "" && len(titles) > 0 && !listed:
			continue
		case u.URL == "" && len(titles) == 0 && i > 0:
			continue
		}
		ex := extract.HTML(c.body, c.ct, c.url)
		if title == "" {
			title = ex.Title
		}
		p := page(u, c.url, c.id, title, ex.Text, c.mime)
		p.Published = ex.Published
		ex.Fill(&p)
		if _, err := w.WriteMetadata(p); err != nil {
			return err
		}
		res.Pages = append(res.Pages, p)
	}
	return nil
}

func readPageTitles(r io.Reader) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var p struct {
			URL   string `json:"url"`
			Title string `json:"title"`
		}
		if jsonUnmarshal(sc.Bytes(), &p) == nil && p.URL != "" {
			out[p.URL] = p.Title
		}
	}
	return out
}
