package archive

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/ogefest/hoarfrost/internal/cdxj"
	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/recorder"
	"github.com/ogefest/hoarfrost/internal/storage"
)

// IndexKey returns the key of the CDXJ index stored next to an incoming
// WARC file, so that open weeks are readable by standard tools.
func IndexKey(warcKey string) string {
	return strings.TrimSuffix(warcKey, ".warc.gz") + ".cdxj"
}

// WriteCaptureIndex stores the CDXJ index of a capture next to its WARC file.
func (a *Archive) WriteCaptureIndex(ctx context.Context, cf recorder.CaptureFile) error {
	var b bytes.Buffer
	if err := cdxj.Write(&b, cf.CDXJ()); err != nil {
		return err
	}
	return storage.PutBytes(ctx, a.Storage, IndexKey(cf.Key()), b.Bytes())
}

// WriteMissingIndexes writes the CDXJ index of incoming WARC files of open
// epochs that have none (captures stored before indexes were written).
func (a *Archive) WriteMissingIndexes(ctx context.Context) (int, error) {
	cats, err := a.Catalogs(ctx)
	if err != nil {
		return 0, err
	}
	sealed := map[epoch.ID]bool{}
	for _, c := range cats {
		sealed[c.Epoch] = true
	}
	objs, err := a.Storage.List(ctx, "incoming/")
	if err != nil {
		return 0, err
	}
	have := map[string]bool{}
	for _, o := range objs {
		have[o.Key] = true
	}
	n := 0
	for _, o := range objs {
		ep, ok := epoch.FromKey(o.Key)
		if !ok || sealed[ep] || !strings.HasSuffix(o.Key, ".warc.gz") || have[IndexKey(o.Key)] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return n, err
		}
		r, err := a.Storage.Open(ctx, o.Key)
		if err != nil {
			return n, err
		}
		cf, err := recorder.Scan(bufio.NewReaderSize(io.NewSectionReader(r, 0, r.Size()), 1<<20), path.Base(o.Key), ep)
		r.Close()
		if err != nil {
			return n, fmt.Errorf("archive: scan %s: %w", o.Key, err)
		}
		if err := a.WriteCaptureIndex(ctx, cf); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
