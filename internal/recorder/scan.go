package recorder

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/warcio"
)

// Scan reconstructs the description of a capture file from its WARC
// content: every record with its position, and pages from hoarfrost metadata
// records. It is how the index is rebuilt from storage.
func Scan(r io.Reader, name string, ep epoch.ID) (CaptureFile, error) {
	cf := CaptureFile{Name: name, Epoch: ep}
	rd := warcio.NewReader(r)
	for {
		rec, off, n, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return cf, err
		}
		cf.Size = off + n
		info := describe(rec, off, n)
		cf.Records = append(cf.Records, info)
		if rec.Type() == warcio.TypeMetadata && IsMetadataContentType(rec.Header.Get(warcio.HContentType)) {
			var p PageInfo
			if err := json.Unmarshal(rec.Block, &p); err == nil {
				cf.Pages = append(cf.Pages, CapturedPage{MetadataID: rec.ID(), Offset: off, Length: n, PageInfo: p})
			}
		}
	}
	return cf, nil
}

// IsMetadataContentType reports whether ct denotes hoarfrost capture metadata,
// including records written while the project was called bindery.
func IsMetadataContentType(ct string) bool {
	return strings.HasPrefix(ct, "application/json") &&
		(strings.Contains(ct, "hoarfrost/capture-metadata") || strings.Contains(ct, "bindery/capture-metadata"))
}

func describe(rec *warcio.Record, off, n int64) RecordInfo {
	date, _ := rec.Date()
	info := RecordInfo{
		ID:       rec.ID(),
		Type:     rec.Type(),
		URI:      rec.TargetURI(),
		Date:     date,
		Digest:   rec.Header.Get(warcio.HPayloadDigest),
		RefersTo: rec.Header.Get(warcio.HRefersTo),
		Offset:   off,
		Length:   n,
	}
	ct := rec.Header.Get(warcio.HContentType)
	switch info.Type {
	case warcio.TypeResponse, warcio.TypeRevisit:
		if strings.HasPrefix(ct, "application/http") {
			if resp, err := warcio.ParseHTTPResponse(rec.Block); err == nil {
				info.Status = resp.StatusCode
				info.Mime = mediaType(resp.Header.Get("Content-Type"))
			}
		} else {
			info.Mime = mediaType(ct)
		}
	case warcio.TypeResource:
		info.Mime = mediaType(ct)
	case warcio.TypeMetadata:
		info.Mime = "application/json"
	}
	return info
}
