package auth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// APIKeyPrefix marks read-only API keys (user tokens use TokenPrefix).
const APIKeyPrefix = "hfr_"

// APIKey is a read-only key for an application (without its secret). Keys
// belong to the instance, not to a user, and only read the archive.
type APIKey struct {
	ID        int64
	Name      string
	Created   time.Time
	CreatedBy string
	Expires   time.Time // zero: never
	LastUsed  time.Time
}

// Expired reports whether the key is past its expiry time.
func (k APIKey) Expired(now time.Time) bool { return !k.Expires.IsZero() && !now.Before(k.Expires) }

// CreateAPIKey issues a read-only API key. The plaintext is returned once;
// only its hash is stored. A zero expires means the key does not expire.
func (s *Store) CreateAPIKey(ctx context.Context, name, createdBy string, expires time.Time) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("auth: API key name is required")
	}
	var exp any
	if !expires.IsZero() {
		exp = expires.UTC()
	}
	key := APIKeyPrefix + randomToken(32)
	_, err := s.db.ExecContext(ctx, `INSERT INTO api_keys(key_hash, name, created, created_by, expires) VALUES ($1,$2,$3,$4,$5)`,
		digest(key), name, s.Now(), createdBy, exp)
	return key, err
}

// CheckAPIKey returns a valid, unexpired API key and records its use.
func (s *Store) CheckAPIKey(ctx context.Context, key string) (APIKey, error) {
	if !strings.HasPrefix(key, APIKeyPrefix) {
		return APIKey{}, ErrNotFound
	}
	k, err := scanAPIKey(s.db.QueryRowContext(ctx, `SELECT `+apiKeyCols+` FROM api_keys WHERE key_hash = $1`, digest(key)))
	if err != nil {
		return APIKey{}, err
	}
	if k.Expired(s.Now()) {
		return APIKey{}, ErrNotFound
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE api_keys SET last_used = $1 WHERE id = $2`, s.Now(), k.ID)
	return k, nil
}

// APIKeys lists all API keys, newest first.
func (s *Store) APIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+apiKeyCols+` FROM api_keys ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// DeleteAPIKey revokes an API key.
func (s *Store) DeleteAPIKey(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_keys WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

const apiKeyCols = `id, name, created, created_by, expires, last_used`

func scanAPIKey(row interface{ Scan(...any) error }) (APIKey, error) {
	var k APIKey
	var expires, used sql.NullTime
	err := row.Scan(&k.ID, &k.Name, &k.Created, &k.CreatedBy, &expires, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return k, ErrNotFound
	}
	k.Created, k.Expires, k.LastUsed = k.Created.UTC(), expires.Time.UTC(), used.Time.UTC()
	return k, err
}
