package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/pgdb"
	"github.com/ogefest/hoarfrost/internal/recorder"
)

// PublishedFix is a page whose publication date changes.
type PublishedFix struct {
	PageID  int64
	Epoch   epoch.ID
	URI     string
	Source  string
	Old     time.Time // zero: unknown
	New     time.Time
	urlID   int64
	storage string
}

// RepairPublished recomputes the publication date of indexed pages from the
// structured data kept in the index (schema.org, Open Graph, microformats),
// so that pages indexed by an older version pick up a fixed date
// extraction without reading the packages. The date found by the text
// extractor is not in the index: a page whose structured data states no
// date keeps its date. Without dryRun the pages and their URLs are
// updated. An empty storage or epoch means all of them.
func (d *DB) RepairPublished(ctx context.Context, storage string, ep epoch.ID, dryRun bool, progress func(scanned int)) ([]PublishedFix, error) {
	var fixes []PublishedFix
	var last int64
	scanned := 0
	for {
		batch, n, err := d.publishedBatch(ctx, storage, ep, last)
		if err != nil {
			return fixes, err
		}
		if n == 0 {
			break
		}
		scanned += n
		last = batch.last
		if !dryRun && len(batch.fixes) > 0 {
			if err := d.applyPublished(ctx, batch.fixes); err != nil {
				return fixes, err
			}
		}
		fixes = append(fixes, batch.fixes...)
		if progress != nil {
			progress(scanned)
		}
	}
	return fixes, nil
}

type publishedBatch struct {
	fixes []PublishedFix
	last  int64
}

const repairBatch = 2000

func (d *DB) publishedBatch(ctx context.Context, storage string, ep epoch.ID, after int64) (publishedBatch, int, error) {
	q := `SELECT page_id, url_id, storage, epoch, uri, coalesce(source,''), published,
	  schema_org::text, open_graph::text, microformats::text
	FROM pages WHERE page_id > ? AND (schema_org IS NOT NULL OR open_graph IS NOT NULL OR microformats IS NOT NULL)`
	args := []any{after}
	if storage != "" {
		q += ` AND storage = ?`
		args = append(args, storage)
	}
	if ep != "" {
		q += ` AND epoch = ?`
		args = append(args, string(ep))
	}
	q += ` ORDER BY page_id LIMIT ?`
	args = append(args, repairBatch)
	rows, err := d.db.QueryContext(ctx, pgdb.Rebind(q), args...)
	if err != nil {
		return publishedBatch{}, 0, err
	}
	defer rows.Close()
	var b publishedBatch
	n := 0
	for rows.Next() {
		var (
			f          PublishedFix
			epo        string
			old        sql.NullTime
			so, og, mf sql.NullString
			p          recorder.PageInfo
		)
		if err := rows.Scan(&f.PageID, &f.urlID, &f.storage, &epo, &f.URI, &f.Source, &old, &so, &og, &mf); err != nil {
			return b, n, err
		}
		n++
		b.last = f.PageID
		if so.Valid {
			_ = json.Unmarshal([]byte(so.String), &p.SchemaOrg)
		}
		if og.Valid {
			_ = json.Unmarshal([]byte(og.String), &p.OpenGraph)
		}
		if mf.Valid {
			_ = json.Unmarshal([]byte(mf.String), &p.Microformats)
		}
		t, ok := published(p).(time.Time)
		if !ok {
			continue
		}
		if old.Valid && old.Time.Equal(t) {
			continue
		}
		f.Epoch = epoch.ID(epo)
		if old.Valid {
			f.Old = old.Time.UTC()
		}
		f.New = t
		b.fixes = append(b.fixes, f)
	}
	return b, n, rows.Err()
}

// applyPublished stores new publication dates of pages and copies them to
// their URLs when the page is the URL's latest capture.
func (d *DB) applyPublished(ctx context.Context, fixes []PublishedFix) error {
	return d.write(ctx, func(tx *sql.Tx) error {
		for _, f := range fixes {
			if _, err := tx.ExecContext(ctx, `UPDATE pages SET published = $1 WHERE page_id = $2`, f.New, f.PageID); err != nil {
				return err
			}
		}
		urls := map[int64]bool{}
		for _, f := range fixes {
			if urls[f.urlID] {
				continue
			}
			urls[f.urlID] = true
			if _, err := tx.ExecContext(ctx, `UPDATE urls u SET published = (
				SELECT p.published FROM pages p WHERE p.url_id = u.id ORDER BY p.captured DESC, p.page_id DESC LIMIT 1)
				WHERE u.id = $1`, f.urlID); err != nil {
				return err
			}
		}
		return nil
	})
}
