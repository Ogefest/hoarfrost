// Package archive implements operations on the archive as a whole: reading
// records, sealing epochs into WACZ packages, verification, the storage
// catalog and README, and rebuilding the index from storage.
package archive

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/recorder"
	"github.com/ogefest/hoarfrost/internal/storage"
	"github.com/ogefest/hoarfrost/internal/warcio"
)

// Archive ties one storage (source of truth) to the local index.
type Archive struct {
	// Name identifies the storage in the index (index.DefaultStorage when empty).
	Name     string
	Storage  storage.Storage
	Index    *index.DB
	SpoolDir string
	Log      *slog.Logger
	Now      func() time.Time
	// IncomingRetention delays deletion of packaged incoming files.
	IncomingRetention time.Duration
	// ConcatBySource joins captures of one source into a single WARC file.
	ConcatBySource bool
	// VolumeSize bounds the capture bytes of one volume of a week
	// (DefaultVolumeSize when 0). A volume is built in SpoolDir and stored
	// with one request, so SpoolDir needs about this much free space.
	VolumeSize int64

	mu      sync.Mutex
	readers map[string]*list.Element
	lru     *list.List
	sealMu  sync.Mutex
}

// New creates an Archive.
func New(st storage.Storage, idx *index.DB, spool string, log *slog.Logger) *Archive {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Archive{Storage: st, Index: idx, SpoolDir: spool, Log: log, Now: time.Now,
		IncomingRetention: 30 * 24 * time.Hour, readers: map[string]*list.Element{}, lru: list.New()}
}

// DefaultVolumeSize is the default capture bytes of one volume of a week.
const DefaultVolumeSize = 4 << 30

func (a *Archive) volumeSize() int64 {
	if a.VolumeSize > 0 {
		return a.VolumeSize
	}
	return DefaultVolumeSize
}

type cachedReader struct {
	key string
	r   storage.Reader
}

const maxOpenReaders = 32

func (a *Archive) name() string { return index.StorageOr(a.Name) }

// reader returns a cached random-access reader for an object.
func (a *Archive) reader(ctx context.Context, key string) (storage.Reader, error) {
	a.mu.Lock()
	if el, ok := a.readers[key]; ok {
		a.lru.MoveToFront(el)
		a.mu.Unlock()
		return el.Value.(*cachedReader).r, nil
	}
	a.mu.Unlock()
	r, err := a.Storage.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if el, ok := a.readers[key]; ok {
		r.Close()
		return el.Value.(*cachedReader).r, nil
	}
	a.readers[key] = a.lru.PushFront(&cachedReader{key, r})
	for a.lru.Len() > maxOpenReaders {
		old := a.lru.Remove(a.lru.Back()).(*cachedReader)
		delete(a.readers, old.key)
		old.r.Close()
	}
	return r, nil
}

func (a *Archive) forget(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if el, ok := a.readers[key]; ok {
		a.lru.Remove(el)
		delete(a.readers, key)
		el.Value.(*cachedReader).r.Close()
	}
}

// Close releases cached readers.
func (a *Archive) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, el := range a.readers {
		el.Value.(*cachedReader).r.Close()
	}
	a.readers = map[string]*list.Element{}
	a.lru.Init()
}

// RawRecord returns the stored (gzip-compressed) bytes of a record.
func (a *Archive) RawRecord(ctx context.Context, r index.Record) ([]byte, error) {
	rd, err := a.reader(ctx, r.Location)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, r.Length)
	if n, err := rd.ReadAt(buf, r.Offset); n < len(buf) {
		return nil, fmt.Errorf("archive: read %s: %w", r.ID, err)
	}
	return buf, nil
}

// ReadRecord reads and parses a record with one ranged read.
func (a *Archive) ReadRecord(ctx context.Context, r index.Record) (*warcio.Record, error) {
	return a.readAt(ctx, r.ID, r.Location, r.Offset, r.Length)
}

func (a *Archive) readAt(ctx context.Context, id, location string, offset, length int64) (*warcio.Record, error) {
	rd, err := a.reader(ctx, location)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, length)
	if n, err := rd.ReadAt(buf, offset); n < len(buf) {
		return nil, fmt.Errorf("archive: read %s at %s:%d: %w", id, location, offset, err)
	}
	rec, err := warcio.ReadRecordAt(bytes.NewReader(buf), 0, length)
	if err != nil {
		return nil, fmt.Errorf("archive: read %s at %s:%d: %w", id, location, offset, err)
	}
	if rec.ID() != id {
		return nil, fmt.Errorf("archive: record at %s:%d is %s, expected %s", location, offset, rec.ID(), id)
	}
	return rec, nil
}

// PageMetadata reads the metadata record of a page: extracted text, title
// and capture details.
func (a *Archive) PageMetadata(ctx context.Context, p index.Page) (*recorder.PageInfo, error) {
	rec, err := a.readAt(ctx, p.MetadataID, p.Location, p.Offset, p.Length)
	if err != nil {
		return nil, err
	}
	var info recorder.PageInfo
	if err := json.Unmarshal(rec.Block, &info); err != nil {
		return nil, fmt.Errorf("archive: metadata %s: %w", p.MetadataID, err)
	}
	return &info, nil
}

// Resource is a replayable capture: HTTP metadata of the requested record and
// the decoded payload of the record holding the content.
type Resource struct {
	Record      index.Record
	PayloadFrom index.Record
	URI         string
	Status      int
	Header      http.Header
	ContentType string
	Body        []byte
}

// Fetch resolves revisits and returns the content of a record.
func (a *Archive) Fetch(ctx context.Context, r index.Record) (*Resource, error) {
	rec, err := a.ReadRecord(ctx, r)
	if err != nil {
		return nil, err
	}
	res := &Resource{Record: r, PayloadFrom: r, URI: r.URI, Header: http.Header{}}
	payloadRec := rec
	if r.Type == warcio.TypeRevisit && r.RefersTo != "" {
		orig, err := a.Index.Payload(ctx, r)
		if err != nil {
			return nil, err
		}
		if payloadRec, err = a.ReadRecord(ctx, orig); err != nil {
			return nil, err
		}
		res.PayloadFrom = orig
	}
	switch rec.Type() {
	case warcio.TypeResource:
		res.Status = http.StatusOK
		res.ContentType = rec.Header.Get(warcio.HContentType)
		res.Header.Set("Content-Type", res.ContentType)
		res.Body = rec.Block
		return res, nil
	case warcio.TypeResponse, warcio.TypeRevisit:
	default:
		return nil, fmt.Errorf("archive: record %s has type %s", r.ID, rec.Type())
	}
	if payloadRec.Type() == warcio.TypeResource {
		res.Status = http.StatusOK
		res.ContentType = payloadRec.Header.Get(warcio.HContentType)
		res.Header.Set("Content-Type", res.ContentType)
		res.Body = payloadRec.Block
		return res, nil
	}
	head, err := warcio.ParseHTTPResponse(rec.Block)
	if err != nil {
		return nil, fmt.Errorf("archive: record %s: %w", r.ID, err)
	}
	res.Status, res.Header = head.StatusCode, head.Header
	body := head.Payload
	encoding := head.Header.Get("Content-Encoding")
	if payloadRec != rec {
		orig, err := warcio.ParseHTTPResponse(payloadRec.Block)
		if err != nil {
			return nil, err
		}
		body = orig.Payload
		encoding = orig.Header.Get("Content-Encoding")
		if res.Header.Get("Content-Type") == "" {
			res.Header.Set("Content-Type", orig.Header.Get("Content-Type"))
		}
	}
	if decoded, ok := recorder.Decode(encoding, body); ok {
		body = decoded
		res.Header.Del("Content-Encoding")
	}
	res.Header.Del("Transfer-Encoding")
	res.Header.Del("Content-Length")
	res.ContentType = res.Header.Get("Content-Type")
	res.Body = body
	return res, nil
}
