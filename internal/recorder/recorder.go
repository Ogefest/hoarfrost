// Package recorder writes everything that ends up in a capture WARC file:
// raw HTTP exchanges, revisits decided by deduplication, resources and
// capture metadata.
package recorder

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"

	"github.com/ogefest/hoarfrost/internal/cdxj"
	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/version"
	"github.com/ogefest/hoarfrost/internal/warcio"
)

// Ref points at an original record for a revisit.
type Ref struct {
	RecordID string
	URI      string
	Date     time.Time
}

// Dedup decides whether a payload was already stored in the epoch.
// Implementations must only return records that are committed to storage.
type Dedup interface {
	Lookup(ctx context.Context, ep epoch.ID, digest string) (*Ref, error)
}

// NoDedup never finds duplicates.
type NoDedup struct{}

func (NoDedup) Lookup(context.Context, epoch.ID, string) (*Ref, error) { return nil, nil }

// SensitiveRequestHeaders are removed from request records.
var SensitiveRequestHeaders = []string{"Cookie", "Authorization", "Proxy-Authorization"}

// SensitiveResponseHeaders are removed from records coming from browsers.
var SensitiveResponseHeaders = []string{"Set-Cookie", "Set-Cookie2"}

// Options configures a Recorder.
type Options struct {
	// Storage names the archive the capture is written to.
	Storage  string
	Epoch    epoch.ID
	Dedup    Dedup
	SpoolDir string
	Source   string
	Engine   string
	Worker   string
	// UserAgent is sent when the request has none.
	UserAgent string
	// MaxResponseSize limits the raw bytes kept per response; longer
	// responses are recorded truncated.
	MaxResponseSize int64
	DialTimeout     time.Duration
	ResponseTimeout time.Duration
	RequestTimeout  time.Duration
	// Blocked reports whether a host must not be contacted.
	Blocked func(host string) bool
	// Info adds fields to the warcinfo record (e.g. block list version).
	Info        map[string]string
	InsecureTLS bool
	TLSConfig   *tls.Config
	// Dial overrides network dialing (tests).
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	Now  func() time.Time
}

// RecordInfo describes one written record.
type RecordInfo struct {
	ID       string    `json:"id"`
	Type     string    `json:"type"`
	URI      string    `json:"uri"`
	Date     time.Time `json:"date"`
	Mime     string    `json:"mime,omitempty"`
	Status   int       `json:"status,omitempty"`
	Digest   string    `json:"digest,omitempty"`
	RefersTo string    `json:"refersTo,omitempty"`
	Offset   int64     `json:"offset"`
	Length   int64     `json:"length"`
}

// Indexable reports whether the record belongs in CDXJ and the record index.
func (r RecordInfo) Indexable() bool {
	switch r.Type {
	case warcio.TypeResponse, warcio.TypeRevisit, warcio.TypeResource:
		return true
	}
	return false
}

// CDXJ returns the index line for the record in the given file.
func (r RecordInfo) CDXJ(filename string) cdxj.Line {
	l := cdxj.Line{
		URL:       r.URI,
		Timestamp: r.Date,
		Mime:      r.Mime,
		Digest:    r.Digest,
		Length:    strconv.FormatInt(r.Length, 10),
		Offset:    strconv.FormatInt(r.Offset, 10),
		Filename:  filename,
		RecordID:  r.ID,
		Type:      r.Type,
		RefersTo:  r.RefersTo,
	}
	if r.Status != 0 {
		l.Status = strconv.Itoa(r.Status)
	}
	if r.Type == warcio.TypeRevisit {
		l.Mime = "warc/revisit"
	}
	return l
}

// CaptureFile is the closed WARC file of one capture.
type CaptureFile struct {
	Path     string         `json:"-"`
	Name     string         `json:"name"`
	Size     int64          `json:"size"`
	Storage  string         `json:"storage,omitempty"`
	Epoch    epoch.ID       `json:"epoch"`
	Records  []RecordInfo   `json:"records"`
	Pages    []CapturedPage `json:"pages"`
	Duration time.Duration  `json:"duration"`
}

// Key returns the storage key under incoming/.
func (c CaptureFile) Key() string { return c.Epoch.IncomingPrefix() + c.Name }

// CDXJ returns the indexable records as CDXJ lines.
func (c CaptureFile) CDXJ() []cdxj.Line {
	var out []cdxj.Line
	for _, r := range c.Records {
		if r.Indexable() {
			out = append(out, r.CDXJ(c.Name))
		}
	}
	return out
}

// Recorder writes one capture WARC file. It is safe for concurrent use.
type Recorder struct {
	opts       Options
	file       *os.File
	path       string
	name       string
	mu         sync.Mutex
	w          *warcio.Writer
	warcinfoID string
	records    []RecordInfo
	pages      []CapturedPage
	local      map[string]Ref
	started    time.Time
	closed     bool
	blocked    int
}

// New creates the spool file and writes the warcinfo record.
func New(opts Options) (*Recorder, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Dedup == nil {
		opts.Dedup = NoDedup{}
	}
	if opts.Epoch == "" {
		opts.Epoch = epoch.Of(opts.Now())
	}
	if opts.UserAgent == "" {
		opts.UserAgent = "Mozilla/5.0 (compatible; " + version.Software() + ")"
	}
	if opts.DialTimeout == 0 {
		opts.DialTimeout = 15 * time.Second
	}
	if opts.ResponseTimeout == 0 {
		opts.ResponseTimeout = 30 * time.Second
	}
	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = 2 * time.Minute
	}
	if opts.MaxResponseSize == 0 {
		opts.MaxResponseSize = 100 << 20
	}
	if opts.SpoolDir == "" {
		opts.SpoolDir = os.TempDir()
	}
	if err := os.MkdirAll(opts.SpoolDir, 0o755); err != nil {
		return nil, err
	}
	now := opts.Now()
	name := FileName(now, opts.Source)
	path := filepath.Join(opts.SpoolDir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	r := &Recorder{opts: opts, file: f, path: path, name: name, w: warcio.NewWriter(f),
		local: map[string]Ref{}, started: now}
	if err := r.writeWarcinfo(now); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	return r, nil
}

// FileName builds "<ts>-<source>-<id>.warc.gz".
func FileName(t time.Time, source string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	src := sanitize(source)
	if src == "" {
		src = "capture"
	}
	return fmt.Sprintf("%s-%s-%s.warc.gz", t.UTC().Format("20060102T150405Z"), src, hex.EncodeToString(b[:]))
}

func sanitize(s string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_':
			b.WriteRune(c)
		case c == '-' || c == '.' || c == ' ':
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

// Epoch returns the epoch the capture belongs to.
func (r *Recorder) Epoch() epoch.ID { return r.opts.Epoch }

// Name returns the WARC file name.
func (r *Recorder) Name() string { return r.name }

func (r *Recorder) writeWarcinfo(now time.Time) error {
	rec := warcio.New(warcio.TypeWarcinfo, now)
	rec.Header.Set(warcio.HFilename, r.name)
	rec.Header.Set(warcio.HContentType, "application/warc-fields")
	fields := [][2]string{
		{"software", version.Software()},
		{"format", "WARC File Format 1.1"},
		{"conformsTo", "http://iipc.github.io/warc-specifications/specifications/warc-format/warc-1.1/"},
		{"engine", r.opts.Engine},
		{"worker", r.opts.Worker},
		{"epoch", string(r.opts.Epoch)},
		{"source", r.opts.Source},
	}
	keys := make([]string, 0, len(r.opts.Info))
	for k := range r.opts.Info {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fields = append(fields, [2]string{k, r.opts.Info[k]})
	}
	var b bytes.Buffer
	for _, f := range fields {
		if f[1] != "" {
			fmt.Fprintf(&b, "%s: %s\r\n", f[0], f[1])
		}
	}
	rec.Block = b.Bytes()
	r.warcinfoID = rec.ID()
	_, err := r.write(rec, "")
	return err
}

func (r *Recorder) write(rec *warcio.Record, mimeType string) (RecordInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return RecordInfo{}, errors.New("recorder: closed")
	}
	if r.warcinfoID != "" && rec.Type() != warcio.TypeWarcinfo {
		rec.Header.Set(warcio.HWarcinfoID, r.warcinfoID)
	}
	rec.Finalize()
	off, n, err := r.w.WriteRecord(rec)
	if err != nil {
		return RecordInfo{}, err
	}
	date, _ := rec.Date()
	info := RecordInfo{
		ID:       rec.ID(),
		Type:     rec.Type(),
		URI:      rec.TargetURI(),
		Date:     date,
		Mime:     mimeType,
		Digest:   rec.Header.Get(warcio.HPayloadDigest),
		RefersTo: rec.Header.Get(warcio.HRefersTo),
		Offset:   off,
		Length:   n,
	}
	r.records = append(r.records, info)
	return info, nil
}

func (r *Recorder) lookup(ctx context.Context, digest string) (*Ref, error) {
	r.mu.Lock()
	ref, ok := r.local[digest]
	r.mu.Unlock()
	if ok {
		return &ref, nil
	}
	return r.opts.Dedup.Lookup(ctx, r.opts.Epoch, digest)
}

func (r *Recorder) remember(digest string, ref Ref) {
	r.mu.Lock()
	if _, ok := r.local[digest]; !ok {
		r.local[digest] = ref
	}
	r.mu.Unlock()
}

// Client returns an HTTP client whose every exchange is recorded.
func (r *Recorder) Client() *http.Client {
	return &http.Client{Transport: r, Timeout: r.opts.RequestTimeout}
}

// Exchange is the result of a recorded HTTP exchange.
type Exchange struct {
	RecordID  string
	Revisit   bool
	Truncated bool
}

// RoundTrip performs and records one HTTP exchange. The returned response
// has its body fully buffered and content-decoded.
func (r *Recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return nil, fmt.Errorf("recorder: unsupported scheme %q", req.URL.Scheme)
	}
	if r.opts.Blocked != nil && r.opts.Blocked(req.URL.Hostname()) {
		r.mu.Lock()
		r.blocked++
		r.mu.Unlock()
		return nil, &BlockedError{Host: req.URL.Hostname()}
	}
	req = req.Clone(req.Context())
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", r.opts.UserAgent)
	}
	if req.Header.Get("Accept-Encoding") == "" {
		req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	}
	date := r.opts.Now()
	c := &capture{limit: r.opts.MaxResponseSize + maxHeaderBytes}
	tr := transportFor(c, &r.opts)
	defer tr.CloseIdleConnections()
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, r.opts.MaxResponseSize+1))
	resp.Body.Close()
	sent, received, truncated, ip := c.snapshot()
	if readErr != nil {
		if len(received) == 0 {
			return nil, readErr
		}
		truncated = true
	}
	if int64(len(raw)) > r.opts.MaxResponseSize {
		truncated = true
		raw = raw[:r.opts.MaxResponseSize]
	}
	ex, err := r.recordExchange(req.Context(), req.URL.String(), date, ip, sent, received, truncated)
	if err != nil {
		return nil, err
	}

	body, decoded := Decode(resp.Header.Get("Content-Encoding"), raw)
	if decoded {
		resp.Header.Del("Content-Encoding")
		resp.Header.Del("Content-Length")
		resp.Uncompressed = true
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Request = req
	resp.Header.Set(HeaderRecordID, ex.RecordID)
	if ex.Truncated {
		resp.Header.Set(HeaderTruncated, "1")
	}
	return resp, nil
}

const maxHeaderBytes = 1 << 20

// Headers added to responses returned by the recording client.
const (
	HeaderRecordID  = "X-Hoarfrost-Record-Id"
	HeaderTruncated = "X-Hoarfrost-Truncated"
)

// BlockedError is returned for requests to blocked hosts.
type BlockedError struct{ Host string }

func (e *BlockedError) Error() string { return "recorder: host blocked: " + e.Host }

// Decode removes a gzip, deflate or br content-encoding. It reports false
// and returns b unchanged for other encodings or undecodable input.
func Decode(encoding string, b []byte) ([]byte, bool) {
	var rd io.Reader
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "gzip", "x-gzip":
		gz, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return b, false
		}
		rd = gz
	case "deflate":
		rd = flate.NewReader(bytes.NewReader(b))
	case "br":
		rd = brotli.NewReader(bytes.NewReader(b))
	default:
		return b, false
	}
	out, err := io.ReadAll(rd)
	if err != nil && len(out) == 0 {
		return b, false
	}
	return out, true
}

// recordExchange writes the request record and a response or revisit record.
func (r *Recorder) recordExchange(ctx context.Context, uri string, date time.Time, ip string, sent, received []byte, truncated bool) (Exchange, error) {
	return r.RecordRaw(ctx, RawExchange{
		URI: uri, Date: date, IP: ip, Request: sent, Response: received, Truncated: truncated,
	})
}

// RawExchange is a raw HTTP request/response pair to be recorded.
type RawExchange struct {
	URI       string
	Date      time.Time
	IP        string
	Request   []byte
	Response  []byte
	Truncated bool
	// StripResponse lists response headers to remove (e.g. Set-Cookie for uploads).
	StripResponse []string
}

// RecordRaw records an HTTP exchange given as raw bytes.
func (r *Recorder) RecordRaw(ctx context.Context, ex RawExchange) (Exchange, error) {
	respBlock := ex.Response
	if len(ex.StripResponse) > 0 {
		respBlock = warcio.StripHeaders(respBlock, ex.StripResponse...)
	}
	parsed, perr := warcio.ParseHTTPResponse(respBlock)

	resp := warcio.New(warcio.TypeResponse, ex.Date)
	resp.Header.Set(warcio.HTargetURI, ex.URI)
	if ex.IP != "" {
		resp.Header.Set(warcio.HIPAddress, ex.IP)
	}
	resp.Header.Set(warcio.HContentType, warcio.ContentTypeHTTPResponse)
	status := 0
	mimeType := ""
	if perr == nil {
		status = parsed.StatusCode
		mimeType = mediaType(parsed.Header.Get("Content-Type"))
		resp.Header.Set(warcio.HPayloadDigest, warcio.Digest(respBlock[len(parsed.HeaderBytes):]))
	}
	if ex.Truncated {
		resp.Header.Set(warcio.HTruncated, "length")
	}

	if perr == nil && !ex.Truncated {
		digest := resp.Header.Get(warcio.HPayloadDigest)
		var ref *Ref
		var profile string
		switch {
		case status == http.StatusNotModified:
			profile = warcio.ProfileServerNotModified
		case len(parsed.Payload) > 0:
			var err error
			ref, err = r.lookup(ctx, digest)
			if err != nil {
				return Exchange{}, fmt.Errorf("recorder: dedup: %w", err)
			}
			if ref != nil {
				profile = warcio.ProfileIdenticalPayload
			}
		}
		if profile != "" {
			resp.Header.Set(warcio.HType, warcio.TypeRevisit)
			resp.Header.Set(warcio.HProfile, profile)
			if ref != nil {
				resp.Header.Set(warcio.HRefersTo, ref.RecordID)
				resp.Header.Set(warcio.HRefersToTargetURI, ref.URI)
				resp.Header.Set(warcio.HRefersToDate, warcio.FormatDate(ref.Date))
			}
			respBlock = parsed.HeaderBytes
		}
	}
	resp.Block = respBlock

	if len(ex.Request) > 0 {
		req := warcio.New(warcio.TypeRequest, ex.Date)
		req.Header.Set(warcio.HTargetURI, ex.URI)
		req.Header.Set(warcio.HContentType, warcio.ContentTypeHTTPRequest)
		req.Header.Set(warcio.HConcurrentTo, resp.ID())
		req.Block = warcio.StripHeaders(ex.Request, SensitiveRequestHeaders...)
		if _, err := r.write(req, ""); err != nil {
			return Exchange{}, err
		}
	}
	info, err := r.write(resp, mimeType)
	if err != nil {
		return Exchange{}, err
	}
	r.setStatus(info.ID, status)
	if info.Type == warcio.TypeResponse && perr == nil && !ex.Truncated && len(parsed.Payload) > 0 {
		r.remember(info.Digest, Ref{RecordID: info.ID, URI: ex.URI, Date: ex.Date})
	}
	return Exchange{RecordID: info.ID, Revisit: info.Type == warcio.TypeRevisit, Truncated: ex.Truncated}, nil
}

func (r *Recorder) setStatus(id string, status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.records) - 1; i >= 0; i-- {
		if r.records[i].ID == id {
			r.records[i].Status = status
			return
		}
	}
}

func mediaType(ct string) string {
	if ct == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	}
	return mt
}

// ResourceRecord is content captured without an HTTP layer.
type ResourceRecord struct {
	URI         string
	Date        time.Time
	ContentType string
	Body        []byte
	// ConcurrentTo links the resource to another record of the same capture.
	ConcurrentTo string
}

// WriteResource writes a resource record, or a revisit when the same payload
// already exists in the epoch.
func (r *Recorder) WriteResource(ctx context.Context, res ResourceRecord) (RecordInfo, error) {
	if res.Date.IsZero() {
		res.Date = r.opts.Now()
	}
	digest := warcio.Digest(res.Body)
	rec := warcio.New(warcio.TypeResource, res.Date)
	rec.Header.Set(warcio.HTargetURI, res.URI)
	rec.Header.Set(warcio.HContentType, res.ContentType)
	rec.Header.Set(warcio.HPayloadDigest, digest)
	if res.ConcurrentTo != "" {
		rec.Header.Set(warcio.HConcurrentTo, res.ConcurrentTo)
	}
	rec.Block = res.Body
	if len(res.Body) > 0 {
		ref, err := r.lookup(ctx, digest)
		if err != nil {
			return RecordInfo{}, err
		}
		if ref != nil {
			rec.Header.Set(warcio.HType, warcio.TypeRevisit)
			rec.Header.Set(warcio.HProfile, warcio.ProfileIdenticalPayload)
			rec.Header.Set(warcio.HRefersTo, ref.RecordID)
			rec.Header.Set(warcio.HRefersToTargetURI, ref.URI)
			rec.Header.Set(warcio.HRefersToDate, warcio.FormatDate(ref.Date))
			rec.Block = nil
		}
	}
	info, err := r.write(rec, mediaType(res.ContentType))
	if err != nil {
		return info, err
	}
	if info.Type == warcio.TypeResource && len(res.Body) > 0 {
		r.remember(digest, Ref{RecordID: info.ID, URI: res.URI, Date: res.Date})
	}
	return info, nil
}

// PageInfo describes a captured page; it is stored as the JSON payload of a
// metadata record and used to build pages.jsonl and the search index.
type PageInfo struct {
	URL       string    `json:"url"`
	RecordID  string    `json:"recordId,omitempty"`
	Title     string    `json:"title,omitempty"`
	Text      string    `json:"text,omitempty"`
	Captured  time.Time `json:"ts"`
	Published string    `json:"published,omitempty"`
	// Description is the meta (or Open Graph) description of the page.
	Description string `json:"description,omitempty"`
	// Authors are the people named as author, creator, contributor or
	// editor in the schema.org data, h-entry authors and article:author.
	Authors []string `json:"authors,omitempty"`
	// SchemaOrg holds the top-level schema.org objects of the page (JSON-LD,
	// microdata and RDFa), each with "@type".
	SchemaOrg []map[string]any `json:"schemaOrg,omitempty"`
	// OpenGraph holds Open Graph and Twitter Card properties ("og:title",
	// "article:tag"…): a string, or a list when the property is repeated.
	OpenGraph map[string]any `json:"openGraph,omitempty"`
	// Microformats holds the top-level microformats2 items (h-entry,
	// h-card…) in the mf2 JSON format, without embedded HTML.
	Microformats []map[string]any `json:"microformats,omitempty"`
	// Keywords are the page's tags (schema.org keywords and articleSection,
	// article:tag, p-category, meta keywords).
	Keywords   []string          `json:"keywords,omitempty"`
	Source     string            `json:"source,omitempty"`
	Engine     string            `json:"engine,omitempty"`
	Fallback   bool              `json:"fallback,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	Status     int               `json:"status,omitempty"`
	Mime       string            `json:"mime,omitempty"`
	Resources  int               `json:"resources,omitempty"`
	Failed     int               `json:"failedResources,omitempty"`
	Blocked    int               `json:"blockedRequests,omitempty"`
	DurationMS int64             `json:"durationMs,omitempty"`
	Extra      map[string]string `json:"extra,omitempty"`
}

// CapturedPage is a page together with the ID of its metadata record.
type CapturedPage struct {
	MetadataID string `json:"metadataId"`
	// Offset and Length locate the metadata record in the capture file.
	Offset int64 `json:"metaOffset"`
	Length int64 `json:"metaLength"`
	PageInfo
}

// PageInfoPrefix is prepended to the page URL in WARC-Target-URI of
// metadata records (as Browsertrix does), so replay tools looking up the
// page URL never return the metadata instead of the page.
const PageInfoPrefix = "urn:pageinfo:"

// MetadataContentType identifies hoarfrost capture metadata payloads.
const MetadataContentType = "application/json; profile=\"https://github.com/ogefest/hoarfrost/capture-metadata\""

// WriteMetadata writes a metadata record describing a captured page.
func (r *Recorder) WriteMetadata(p PageInfo) (RecordInfo, error) {
	if p.Captured.IsZero() {
		p.Captured = r.started
	}
	if p.Source == "" {
		p.Source = r.opts.Source
	}
	if p.Engine == "" {
		p.Engine = r.opts.Engine
	}
	r.mu.Lock()
	p.Blocked += r.blocked
	r.blocked = 0
	r.mu.Unlock()
	body, err := json.Marshal(p)
	if err != nil {
		return RecordInfo{}, err
	}
	rec := warcio.New(warcio.TypeMetadata, r.opts.Now())
	rec.Header.Set(warcio.HTargetURI, PageInfoPrefix+p.URL)
	rec.Header.Set(warcio.HContentType, MetadataContentType)
	if p.RecordID != "" {
		rec.Header.Set(warcio.HRefersTo, p.RecordID)
		rec.Header.Set(warcio.HRefersToTargetURI, p.URL)
	}
	rec.Block = body
	info, err := r.write(rec, "application/json")
	if err != nil {
		return info, err
	}
	r.mu.Lock()
	r.pages = append(r.pages, CapturedPage{MetadataID: info.ID, Offset: info.Offset, Length: info.Length, PageInfo: p})
	r.mu.Unlock()
	return info, nil
}

// Records returns a snapshot of the records written so far.
func (r *Recorder) Records() []RecordInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RecordInfo(nil), r.records...)
}

// Close finishes the file and returns its description.
func (r *Recorder) Close() (CaptureFile, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return CaptureFile{}, errors.New("recorder: already closed")
	}
	r.closed = true
	err := r.file.Sync()
	if cerr := r.file.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(r.path)
		return CaptureFile{}, err
	}
	return CaptureFile{
		Path:     r.path,
		Name:     r.name,
		Size:     r.w.Offset(),
		Storage:  r.opts.Storage,
		Epoch:    r.opts.Epoch,
		Records:  append([]RecordInfo(nil), r.records...),
		Pages:    append([]CapturedPage(nil), r.pages...),
		Duration: r.opts.Now().Sub(r.started),
	}, nil
}

// Abort closes and removes the spool file.
func (r *Recorder) Abort() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		r.file.Close()
	}
	os.Remove(r.path)
}
