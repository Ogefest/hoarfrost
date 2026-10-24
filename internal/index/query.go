package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/pgdb"
)

// SearchQuery filters pages.
type SearchQuery struct {
	Q       string
	Storage string
	Source  string
	Domain  string
	Kind    string
	// Keyword matches pages tagged with it, ignoring case and diacritics.
	Keyword string
	From    time.Time
	To      time.Time
	Limit   int
	Offset  int
}

// FTSQuery turns free text into a safe tsquery (for to_tsquery('simple',
// ...)): every word is quoted, all must match and the last one is a prefix.
func FTSQuery(q string) string {
	words := strings.FieldsFunc(Fold(q), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_'
	})
	for i, w := range words {
		words[i] = `'` + strings.ToLower(w) + `'`
		if i == len(words)-1 {
			words[i] += ":*"
		}
	}
	return strings.Join(words, " & ")
}

const pageCols = `p.page_id, p.metadata_id, coalesce(p.record_id,''), p.url_id, p.storage, p.epoch, p.uri, coalesce(p.title,''),
	coalesce(p.description,''), p.captured, p.published, coalesce(array_to_json(p.authors)::text,'[]'),
	coalesce(array_to_json(p.schema_types)::text,'[]'), p.schema_org::text, p.open_graph::text, p.microformats::text,
	coalesce(array_to_json(p.keywords)::text,'[]'), coalesce(p.source,''), coalesce(p.engine,''), coalesce(p.kind,''), coalesce(p.mime,''),
	coalesce(p.status,0), p.fallback, p.text_len, p.resources, p.file, p.location, p."offset", p.length`

func scanPage(row interface{ Scan(...any) error }) (Page, error) {
	var p Page
	var ep string
	var published sql.NullTime
	var schemaOrg, openGraph, microformats sql.NullString
	var authors, types, keywords string
	err := row.Scan(&p.ID, &p.MetadataID, &p.RecordID, &p.URLID, &p.Storage, &ep, &p.URI, &p.Title, &p.Description, &p.Captured, &published,
		&authors, &types, &schemaOrg, &openGraph, &microformats, &keywords, &p.Source, &p.Engine, &p.Kind, &p.Mime, &p.Status, &p.Fallback, &p.TextLen, &p.Resources,
		&p.File, &p.Location, &p.Offset, &p.Length)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	p.Epoch, p.Captured = epoch.ID(ep), p.Captured.UTC()
	_ = json.Unmarshal([]byte(authors), &p.Authors) // written by AddCapture; a bad value only hides the list
	_ = json.Unmarshal([]byte(types), &p.SchemaTypes)
	_ = json.Unmarshal([]byte(keywords), &p.Keywords)
	if openGraph.Valid {
		p.OpenGraph = json.RawMessage(openGraph.String)
	}
	if microformats.Valid {
		p.Microformats = json.RawMessage(microformats.String)
	}
	if published.Valid {
		p.Published = published.Time.UTC().Format(time.DateOnly)
	}
	if schemaOrg.Valid {
		p.SchemaOrg = json.RawMessage(schemaOrg.String)
	}
	return p, err
}

// Search returns matching pages and the total number of matches.
func (d *DB) Search(ctx context.Context, q SearchQuery) ([]Page, int, error) {
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 50
	}
	from, order := `pages p`, `p.captured DESC`
	where, args := q.filters()
	if fts := FTSQuery(q.Q); fts != "" {
		from = `pages p, to_tsquery('simple', ?) q`
		where = append([]string{`p.search @@ q`}, where...)
		args = append([]any{fts}, args...)
		order = `ts_rank(p.search, q) DESC, p.captured DESC`
	}
	cond := ""
	if len(where) > 0 {
		cond = " WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := d.db.QueryRowContext(ctx, pgdb.Rebind(`SELECT count(*) FROM `+from+cond), args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("index: search: %w", err)
	}
	out, err := d.queryPages(ctx, `SELECT `+pageCols+` FROM `+from+cond+` ORDER BY `+order+` LIMIT ? OFFSET ?`, append(args, q.Limit, q.Offset)...)
	if err != nil {
		return nil, 0, fmt.Errorf("index: search: %w", err)
	}
	return out, total, nil
}

// PagesAfter lists pages with an ID greater than after in ID (insertion)
// order, for clients that follow the archive incrementally. Q is ignored.
// Reindexing an epoch inserts its pages again under new IDs.
func (d *DB) PagesAfter(ctx context.Context, after int64, q SearchQuery) ([]Page, error) {
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 100
	}
	where, args := q.filters()
	where = append([]string{`p.page_id > ?`}, where...)
	args = append([]any{after}, args...)
	out, err := d.queryPages(ctx, `SELECT `+pageCols+` FROM pages p WHERE `+strings.Join(where, " AND ")+` ORDER BY p.page_id LIMIT ?`, append(args, q.Limit)...)
	if err != nil {
		return nil, fmt.Errorf("index: pages: %w", err)
	}
	return out, nil
}

// filters returns the conditions of q other than the full-text query.
func (q SearchQuery) filters() ([]string, []any) {
	var where []string
	var args []any
	if q.Source != "" {
		where = append(where, `p.source = ?`)
		args = append(args, q.Source)
	}
	if q.Storage != "" {
		where = append(where, `p.storage = ?`)
		args = append(args, q.Storage)
	}
	if q.Kind != "" {
		where = append(where, `p.kind = ?`)
		args = append(args, q.Kind)
	}
	if k := KeywordKey(q.Keyword); k != "" {
		where = append(where, `EXISTS (SELECT 1 FROM page_keywords k WHERE k.page_id = p.page_id AND k.key = ?)`)
		args = append(args, k)
	}
	if d := strings.ToLower(strings.TrimSpace(q.Domain)); d != "" {
		where = append(where, `(p.uri LIKE ? OR p.uri LIKE ? OR p.uri LIKE ? OR p.uri LIKE ?)`)
		args = append(args, "%://"+d+"/%", "%://"+d, "%."+d+"/%", "%://"+d+":%")
	}
	if !q.From.IsZero() {
		where = append(where, `p.captured >= ?`)
		args = append(args, utc(q.From))
	}
	if !q.To.IsZero() {
		where = append(where, `p.captured < ?`)
		args = append(args, utc(q.To))
	}
	return where, args
}

func (d *DB) queryPages(ctx context.Context, query string, args ...any) ([]Page, error) {
	rows, err := d.db.QueryContext(ctx, pgdb.Rebind(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Page
	for rows.Next() {
		p, err := scanPage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Page returns a page by ID.
func (d *DB) Page(ctx context.Context, id int64) (Page, error) {
	return scanPage(d.db.QueryRowContext(ctx, `SELECT `+pageCols+` FROM pages p WHERE page_id = $1`, id))
}

// PageByMetadata returns a page by its metadata record ID.
func (d *DB) PageByMetadata(ctx context.Context, id string) (Page, error) {
	return scanPage(d.db.QueryRowContext(ctx, `SELECT `+pageCols+` FROM pages p WHERE metadata_id = $1`, id))
}

// EpochPages lists the pages of an epoch ordered by URL and capture time.
func (d *DB) EpochPages(ctx context.Context, storage string, ep epoch.ID) ([]Page, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT `+pageCols+` FROM pages p WHERE storage = $1 AND epoch = $2 ORDER BY uri, captured, metadata_id`,
		StorageOr(storage), string(ep))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Page
	for rows.Next() {
		p, err := scanPage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PageCaptures lists captures of the same URL, newest first.
func (d *DB) PageCaptures(ctx context.Context, uri string) ([]Page, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT `+pageCols+` FROM pages p
		WHERE p.url_id IN (SELECT id FROM urls WHERE uri_md5 = md5($1)::uuid) AND p.uri = $1 ORDER BY captured DESC`, uri)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Page
	for rows.Next() {
		p, err := scanPage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdatePageText replaces the indexed text of a page (e.g. after OCR).
func (d *DB) UpdatePageText(ctx context.Context, pageID int64, title, text string) error {
	return d.write(ctx, func(tx *sql.Tx) error {
		var description string
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(description, '') FROM pages WHERE page_id = $1`, pageID).Scan(&description); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE pages SET text_len = $1, search = `+searchVector(2, 3, 4)+` WHERE page_id = $5`,
			len([]rune(text)), searchText(title), searchText(description), searchText(text), pageID)
		return err
	})
}

// Stats summarizes the archive for the dashboard.
type Stats struct {
	Records    int64
	Pages      int64
	Revisits   int64
	Bytes      int64
	DedupSaved int64
	EpochSizes []EpochSize
	PerDay     []DayCount
	Sources    []SourceStats
	LastEpoch  EpochInfo
}

// EpochSize is the stored size of one epoch.
type EpochSize struct {
	Storage string
	Epoch   epoch.ID
	State   string
	Bytes   int64
	Pages   int64
}

// DayCount is the number of pages captured on a day.
type DayCount struct {
	Day   string
	Pages int64
}

// SourceStats aggregates captures per source.
type SourceStats struct {
	Source    string
	Pages     int64
	Fallbacks int64
	Short     int64
	Last      time.Time
}

// FallbackRatio is the share of pages that needed the fallback engine.
func (s SourceStats) FallbackRatio() float64 {
	if s.Pages == 0 {
		return 0
	}
	return float64(s.Fallbacks) / float64(s.Pages)
}

// Stats computes dashboard numbers; days limits the per-day series.
func (d *DB) Stats(ctx context.Context, days int) (Stats, error) {
	var s Stats
	err := d.db.QueryRowContext(ctx, `SELECT
		(SELECT coalesce(sum(records),0) FROM epoch_stats),
		(SELECT count(*) FROM pages),
		(SELECT coalesce(sum(revisits),0) FROM epoch_stats),
		(SELECT coalesce(sum(saved),0) FROM epoch_stats)`).
		Scan(&s.Records, &s.Pages, &s.Revisits, &s.DedupSaved)
	if err != nil {
		return s, err
	}
	if s.EpochSizes, err = d.EpochSizes(ctx); err != nil {
		return s, err
	}
	for _, e := range s.EpochSizes {
		s.Bytes += e.Bytes
	}
	since := time.Now().UTC().AddDate(0, 0, -days).Truncate(24 * time.Hour)
	if s.PerDay, err = d.perDay(ctx, since); err != nil {
		return s, err
	}
	if s.Sources, err = d.SourceStats(ctx); err != nil {
		return s, err
	}

	eps, err := d.epochs(ctx, `WHERE state IN ('sealed','verified','failed')`)
	if err != nil {
		return s, err
	}
	if len(eps) > 0 {
		s.LastEpoch = eps[0]
	}
	return s, nil
}

// EpochSizes returns the stored size and page count of every epoch, newest first.
func (d *DB) EpochSizes(ctx context.Context) ([]EpochSize, error) {
	rows, err := d.db.QueryContext(ctx, `
		WITH fsz AS (SELECT storage, epoch, sum(size) AS bytes FROM files GROUP BY storage, epoch),
		     pc AS (SELECT storage, epoch, count(*) AS n FROM pages GROUP BY storage, epoch),
		     eps AS (SELECT storage, epoch FROM files UNION SELECT storage, epoch FROM epochs)
		SELECT eps.storage, eps.epoch, coalesce(e.state,'open'),
		       CASE WHEN e.state IN ('sealed','verified') THEN coalesce(e.size,0) ELSE coalesce(fsz.bytes,0) END,
		       coalesce(pc.n,0)
		FROM eps LEFT JOIN epochs e ON e.storage = eps.storage AND e.epoch = eps.epoch
		LEFT JOIN fsz ON fsz.storage = eps.storage AND fsz.epoch = eps.epoch
		LEFT JOIN pc ON pc.storage = eps.storage AND pc.epoch = eps.epoch
		ORDER BY eps.epoch DESC, eps.storage`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EpochSize
	for rows.Next() {
		var e EpochSize
		var ep string
		if err := rows.Scan(&e.Storage, &ep, &e.State, &e.Bytes, &e.Pages); err != nil {
			return nil, err
		}
		e.Epoch = epoch.ID(ep)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (d *DB) perDay(ctx context.Context, since time.Time) ([]DayCount, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT to_char(captured AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day, count(*) FROM pages
		WHERE captured >= $1 GROUP BY day ORDER BY day`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DayCount
	for rows.Next() {
		var dc DayCount
		if err := rows.Scan(&dc.Day, &dc.Pages); err != nil {
			return nil, err
		}
		out = append(out, dc)
	}
	return out, rows.Err()
}

// SourceStats aggregates pages per source.
func (d *DB) SourceStats(ctx context.Context) ([]SourceStats, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT coalesce(source,''), count(*), count(*) FILTER (WHERE fallback),
		sum(CASE WHEN text_len < 200 AND kind = 'page' THEN 1 ELSE 0 END), max(captured)
		FROM pages GROUP BY source ORDER BY source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SourceStats
	for rows.Next() {
		var ss SourceStats
		var last sql.NullTime
		if err := rows.Scan(&ss.Source, &ss.Pages, &ss.Fallbacks, &ss.Short, &last); err != nil {
			return nil, err
		}
		ss.Last = last.Time.UTC()
		out = append(out, ss)
	}
	return out, rows.Err()
}

// Dump renders the reproducible content of the index as sorted text lines.
// Operational columns (row IDs, verification time, when a URL was first
// seen in a feed) are excluded.
func (d *DB) Dump(ctx context.Context) (string, error) {
	var b strings.Builder
	queries := []string{
		`SELECT 'record', record_id, storage, epoch, uri, date::text, type, mime, status, digest, refers_to, location, "offset", length, file FROM records ORDER BY record_id`,
		`SELECT 'page', metadata_id, coalesce(record_id,''), storage, epoch, uri, coalesce(title,''), coalesce(description,''), captured::text,
			coalesce(published::text,''), coalesce(authors::text,''), coalesce(schema_types::text,''), coalesce(schema_org::text,''),
			coalesce(open_graph::text,''), coalesce(microformats::text,''), coalesce(keywords::text,''),
			coalesce(source,''), coalesce(engine,''), coalesce(kind,''), coalesce(mime,''), coalesce(status,0), fallback, text_len, resources, file,
			location, "offset", length
			FROM pages ORDER BY metadata_id`,
		`SELECT 'item', p.metadata_id, i.position, i.format, i.type, i.types::text, coalesce(i.property,''),
			coalesce((SELECT parent.position FROM page_items parent WHERE parent.item_id = i.parent_id), -1), i.data::text
			FROM page_items i JOIN pages p USING (page_id) ORDER BY p.metadata_id, i.position`,
		`SELECT 'property', p.metadata_id, o.format, o.name, o.position, o.value
			FROM page_properties o JOIN pages p USING (page_id) ORDER BY p.metadata_id, o.name, o.position`,
		`SELECT 'keyword', p.metadata_id, k.key, k.keyword, k.position
			FROM page_keywords k JOIN pages p USING (page_id) ORDER BY p.metadata_id, k.key`,
		`SELECT 'epoch', storage, epoch, state, coalesce(wacz_key,''), coalesce(sha256,''), coalesce(size,0) FROM epochs ORDER BY storage, epoch`,
		`SELECT 'url', storage, uri, last_captured::text, coalesce(title,''), coalesce(description,''), coalesce(published::text,''),
			coalesce(authors::text,''), coalesce(schema_types::text,''), coalesce(schema_org::text,'')
			FROM urls WHERE last_captured IS NOT NULL ORDER BY storage, uri`,
	}
	for _, q := range queries {
		if err := d.dumpQuery(ctx, &b, q); err != nil {
			return "", err
		}
	}
	return b.String(), nil
}

func (d *DB) dumpQuery(ctx context.Context, b *strings.Builder, q string) error {
	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		for i, v := range vals {
			if i > 0 {
				b.WriteByte('\t')
			}
			fmt.Fprint(b, v)
		}
		b.WriteByte('\n')
	}
	return rows.Err()
}

// CountMatches returns the number of pages matching a full-text query.
func (d *DB) CountMatches(ctx context.Context, q string) (int, error) {
	_, n, err := d.Search(ctx, SearchQuery{Q: q, Limit: 1})
	return n, err
}

// VolumeRow is the stored volume of one content category.
type VolumeRow struct {
	Category string
	Records  int64
	Revisits int64
	Bytes    int64
}

// Volume breaks down stored record sizes by content category (all epochs
// when ep is empty), to guide format and epoch-length decisions.
func (d *DB) Volume(ctx context.Context, ep epoch.ID) ([]VolumeRow, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT CASE
		  WHEN r.type = 'revisit' THEN coalesce((SELECT CASE
		      WHEN o.mime LIKE 'image/%' THEN 'image' WHEN o.mime LIKE 'font/%' OR o.mime LIKE '%font%' THEN 'font'
		      WHEN o.mime = 'text/css' THEN 'css' WHEN o.mime LIKE '%javascript%' THEN 'javascript'
		      WHEN o.mime LIKE 'text/html%' OR o.mime = 'application/xhtml+xml' THEN 'html'
		      WHEN o.mime = 'application/pdf' OR o.mime LIKE 'application/vnd.%' OR o.mime LIKE 'application/msword%' THEN 'document'
		      WHEN o.mime LIKE 'video/%' OR o.mime LIKE 'audio/%' THEN 'media' ELSE 'other' END
		    FROM records o WHERE o.record_id = r.refers_to), 'other')
		  WHEN r.mime LIKE 'image/%' THEN 'image' WHEN r.mime LIKE 'font/%' OR r.mime LIKE '%font%' THEN 'font'
		  WHEN r.mime = 'text/css' THEN 'css' WHEN r.mime LIKE '%javascript%' THEN 'javascript'
		  WHEN r.mime LIKE 'text/html%' OR r.mime = 'application/xhtml+xml' THEN 'html'
		  WHEN r.mime = 'application/pdf' OR r.mime LIKE 'application/vnd.%' OR r.mime LIKE 'application/msword%' THEN 'document'
		  WHEN r.mime LIKE 'video/%' OR r.mime LIKE 'audio/%' THEN 'media'
		  ELSE 'other' END AS category,
		  count(*), count(*) FILTER (WHERE r.type = 'revisit'), sum(r.length)
		FROM records r WHERE $1 = '' OR r.epoch = $2
		GROUP BY category ORDER BY sum(r.length) DESC`, string(ep), string(ep))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VolumeRow
	for rows.Next() {
		var v VolumeRow
		if err := rows.Scan(&v.Category, &v.Records, &v.Revisits, &v.Bytes); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
