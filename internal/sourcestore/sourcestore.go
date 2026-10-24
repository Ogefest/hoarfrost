// Package sourcestore keeps the capture configuration in app.sqlite: the
// sources, the defaults they inherit, the block list settings and the result
// of the last poll of every source.
package sourcestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ogefest/hoarfrost/internal/config"
	"github.com/ogefest/hoarfrost/internal/pgdb"
)

// Status is the result of the last poll of a source.
type Status struct {
	LastPoll time.Time
	Found    int
	New      int
	Error    string
}

// Store reads and writes the configuration.
type Store struct {
	db  *sql.DB
	Now func() time.Time
}

// New returns a store on the application database (see appdb).
func New(db *sql.DB) *Store { return &Store{db: db, Now: time.Now} }

const (
	keyDefaults   = "defaults"
	keyBlocklists = "blocklists"
)

// Load returns the configuration. An empty database yields no sources, no
// defaults and the default block lists.
func (s *Store) Load(ctx context.Context) (*config.Sources, error) {
	out := &config.Sources{}
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings WHERE key IN ($1, $2)`, keyDefaults, keyBlocklists)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		var dst any = &out.Defaults
		if key == keyBlocklists {
			out.Blocklists = &config.Blocklists{}
			dst = out.Blocklists
		}
		if err := json.Unmarshal([]byte(value), dst); err != nil {
			return nil, fmt.Errorf("sourcestore: setting %s: %w", key, err)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	srows, err := s.db.QueryContext(ctx, `SELECT name, config FROM sources ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer srows.Close()
	for srows.Next() {
		var name, raw string
		if err := srows.Scan(&name, &raw); err != nil {
			return nil, err
		}
		var src config.Source
		if err := json.Unmarshal([]byte(raw), &src); err != nil {
			return nil, fmt.Errorf("sourcestore: source %s: %w", name, err)
		}
		src.Name = name
		out.Sources = append(out.Sources, src)
	}
	return out, srows.Err()
}

// Save replaces the configuration. Poll results of sources that remain are
// kept; removed sources are deleted with theirs.
func (s *Store) Save(ctx context.Context, cfg *config.Sources) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := putSetting(ctx, tx, keyDefaults, cfg.Defaults); err != nil {
		return err
	}
	if cfg.Blocklists == nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = $1`, keyBlocklists); err != nil {
			return err
		}
	} else if err := putSetting(ctx, tx, keyBlocklists, cfg.Blocklists); err != nil {
		return err
	}
	now := s.Now().UTC()
	for _, src := range cfg.Sources {
		b, err := json.Marshal(src)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO sources(name, config, created, updated) VALUES ($1, $2, $3, $4)
ON CONFLICT(name) DO UPDATE SET config = excluded.config, updated = excluded.updated WHERE sources.config <> excluded.config`,
			src.Name, string(b), now, now); err != nil {
			return err
		}
	}
	// Sources missing from cfg are deleted with their poll state.
	del := `DELETE FROM sources`
	var args []any
	if len(cfg.Sources) > 0 {
		del += ` WHERE name NOT IN (?` + strings.Repeat(`, ?`, len(cfg.Sources)-1) + `)`
		for _, src := range cfg.Sources {
			args = append(args, src.Name)
		}
	}
	if _, err := tx.ExecContext(ctx, pgdb.Rebind(del), args...); err != nil {
		return err
	}
	return tx.Commit()
}

func putSetting(ctx context.Context, tx *sql.Tx, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES ($1, $2) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, string(b))
	return err
}

// RecordPoll stores the result of a poll. A source deleted in the meantime
// is ignored.
func (s *Store) RecordPoll(ctx context.Context, name string, st Status) error {
	var errText sql.NullString
	if st.Error != "" {
		errText = sql.NullString{String: st.Error, Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE sources SET last_poll = $1, found = $2, new = $3, last_error = $4 WHERE name = $5`,
		st.LastPoll.UTC(), st.Found, st.New, errText, name)
	return err
}

// Statuses returns the last poll result of every source polled at least once.
func (s *Store) Statuses(ctx context.Context) (map[string]Status, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, last_poll, found, new, last_error FROM sources WHERE last_poll IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Status{}
	for rows.Next() {
		var name string
		var last time.Time
		var st Status
		var errText sql.NullString
		if err := rows.Scan(&name, &last, &st.Found, &st.New, &errText); err != nil {
			return nil, err
		}
		st.LastPoll, st.Error = last.UTC(), errText.String
		out[name] = st
	}
	return out, rows.Err()
}
