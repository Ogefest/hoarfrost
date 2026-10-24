// Package auth manages users, sessions and API tokens stored in app.sqlite.
// Passwords are hashed with argon2id after an HMAC-SHA256 with the instance
// secret (pepper), so a leaked app.sqlite alone cannot be attacked offline.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/ogefest/hoarfrost/internal/pgdb"
)

// Roles.
const (
	RoleAdmin  = "admin"
	RoleEditor = "editor"
	RoleViewer = "viewer"
)

// Can reports whether role grants at least the required role.
func Can(role, required string) bool {
	rank := map[string]int{RoleViewer: 1, RoleEditor: 2, RoleAdmin: 3}
	return rank[role] >= rank[required] && rank[required] > 0
}

// MinPasswordLen is the minimum password length in characters.
const MinPasswordLen = 5

// Errors.
var (
	ErrInvalidCredentials = errors.New("auth: invalid login or password")
	ErrNotFound           = errors.New("auth: not found")
	ErrWeakPassword       = fmt.Errorf("auth: password must have at least %d characters", MinPasswordLen)
)

// Argon2 parameters (variables so tests can lower the cost).
var (
	ArgonTime    uint32 = 3
	ArgonMemory  uint32 = 64 * 1024
	ArgonThreads uint8  = 2
)

// User is an account.
type User struct {
	ID      int64
	Login   string
	Role    string
	Created time.Time
}

// Store gives access to accounts.
type Store struct {
	db     *sql.DB
	pepper []byte
	Now    func() time.Time
}

// New returns a store using secret as the password pepper.
func New(db *sql.DB, secret string) *Store {
	return &Store{db: db, pepper: []byte(secret), Now: time.Now}
}

func (s *Store) pepperize(password string) []byte {
	m := hmac.New(sha256.New, s.pepper)
	m.Write([]byte(password))
	return m.Sum(nil)
}

// HashPassword returns a PHC-formatted argon2id hash.
func (s *Store) HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey(s.pepperize(password), salt, ArgonTime, ArgonMemory, ArgonThreads, 32)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, ArgonMemory, ArgonTime, ArgonThreads,
		enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// CheckPassword verifies a password against a hash.
func (s *Store) CheckPassword(hash, password string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[4])
	want, err2 := enc.DecodeString(parts[5])
	if err1 != nil || err2 != nil {
		return false
	}
	got := argon2.IDKey(s.pepperize(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

var loginRe = regexp.MustCompile(`^[a-zA-Z0-9._@-]{2,64}$`)

// CreateUser adds an account.
func (s *Store) CreateUser(ctx context.Context, login, password, role string) (User, error) {
	if !loginRe.MatchString(login) {
		return User{}, fmt.Errorf("auth: invalid login %q", login)
	}
	if !Can(role, RoleViewer) {
		return User{}, fmt.Errorf("auth: invalid role %q", role)
	}
	if len([]rune(password)) < MinPasswordLen {
		return User{}, ErrWeakPassword
	}
	hash, err := s.HashPassword(password)
	if err != nil {
		return User{}, err
	}
	now := s.Now().UTC()
	var id int64
	err = s.db.QueryRowContext(ctx, `INSERT INTO users(login, pwhash, role, created) VALUES ($1,$2,$3,$4) RETURNING id`,
		login, hash, role, now).Scan(&id)
	if err != nil {
		if pgdb.IsUniqueViolation(err) {
			return User{}, fmt.Errorf("auth: user %q already exists", login)
		}
		return User{}, err
	}
	return User{ID: id, Login: login, Role: role, Created: now}, nil
}

// SetPassword replaces a password and ends the user's sessions.
func (s *Store) SetPassword(ctx context.Context, login, password string) error {
	if len([]rune(password)) < MinPasswordLen {
		return ErrWeakPassword
	}
	hash, err := s.HashPassword(password)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE users SET pwhash = $1 WHERE login = $2`, hash, login)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = (SELECT id FROM users WHERE login = $1)`, login)
	return err
}

// SetRole changes a user's role.
func (s *Store) SetRole(ctx context.Context, login, role string) error {
	if !Can(role, RoleViewer) {
		return fmt.Errorf("auth: invalid role %q", role)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE users SET role = $1 WHERE login = $2`, role, login)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUser removes an account with its sessions and tokens.
func (s *Store) DeleteUser(ctx context.Context, login string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE login = $1`, login)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Users lists accounts.
func (s *Store) Users(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, login, role, created FROM users ORDER BY login`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Login, &u.Role, &u.Created); err != nil {
			return nil, err
		}
		u.Created = u.Created.UTC()
		out = append(out, u)
	}
	return out, rows.Err()
}

// CountUsers returns the number of accounts.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

// dummyHash equalizes timing for unknown logins.
var dummyHash = "$argon2id$v=19$m=65536,t=3,p=2$c29tZXNhbHRzb21lc2FsdA$Zm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyZm9vYmFyMTI"

// Authenticate checks login and password.
func (s *Store) Authenticate(ctx context.Context, login, password string) (User, error) {
	var u User
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT id, login, role, created, pwhash FROM users WHERE login = $1`, login).
		Scan(&u.ID, &u.Login, &u.Role, &u.Created, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		s.CheckPassword(dummyHash, password)
		return User{}, ErrInvalidCredentials
	}
	if err != nil {
		return User{}, err
	}
	if !s.CheckPassword(hash, password) {
		return User{}, ErrInvalidCredentials
	}
	u.Created = u.Created.UTC()
	return u, nil
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func digest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Session is an authenticated browser session.
type Session struct {
	ID      string
	User    User
	CSRF    string
	Expires time.Time
}

// CreateSession starts a session; the returned ID goes into the cookie and
// only its hash is stored.
func (s *Store) CreateSession(ctx context.Context, u User, ttl time.Duration) (Session, error) {
	sess := Session{ID: randomToken(32), User: u, CSRF: randomToken(24), Expires: s.Now().Add(ttl).UTC()}
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions(id, user_id, expires, csrf) VALUES ($1,$2,$3,$4)`,
		digest(sess.ID), u.ID, sess.Expires, sess.CSRF)
	return sess, err
}

// Session returns a valid session by cookie value.
func (s *Store) Session(ctx context.Context, id string) (Session, error) {
	var sess Session
	err := s.db.QueryRowContext(ctx, `SELECT u.id, u.login, u.role, u.created, s.expires, s.csrf
		FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.id = $1`, digest(id)).
		Scan(&sess.User.ID, &sess.User.Login, &sess.User.Role, &sess.User.Created, &sess.Expires, &sess.CSRF)
	if errors.Is(err, sql.ErrNoRows) {
		return sess, ErrNotFound
	}
	if err != nil {
		return sess, err
	}
	sess.ID = id
	sess.Expires, sess.User.Created = sess.Expires.UTC(), sess.User.Created.UTC()
	if s.Now().After(sess.Expires) {
		_ = s.DeleteSession(ctx, id)
		return Session{}, ErrNotFound
	}
	return sess, nil
}

// DeleteSession ends a session.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = $1`, digest(id))
	return err
}

// PurgeSessions removes expired sessions.
func (s *Store) PurgeSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires < $1`, s.Now())
	return err
}

// TokenPrefix marks hoarfrost API tokens.
const TokenPrefix = "hf_"

// Token is an API token (without its secret).
type Token struct {
	ID       int64
	UserID   int64
	Name     string
	Created  time.Time
	LastUsed time.Time
}

// CreateToken issues an API token. The plaintext is returned once; only its
// hash is stored.
func (s *Store) CreateToken(ctx context.Context, u User, name string) (string, error) {
	tok := TokenPrefix + randomToken(32)
	_, err := s.db.ExecContext(ctx, `INSERT INTO api_tokens(user_id, token_hash, name, created) VALUES ($1,$2,$3,$4)`,
		u.ID, digest(tok), name, s.Now())
	return tok, err
}

// TokenUser returns the owner of a valid API token.
func (s *Store) TokenUser(ctx context.Context, tok string) (User, error) {
	if !strings.HasPrefix(tok, TokenPrefix) {
		return User{}, ErrNotFound
	}
	var u User
	err := s.db.QueryRowContext(ctx, `SELECT u.id, u.login, u.role, u.created FROM api_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = $1`, digest(tok)).Scan(&u.ID, &u.Login, &u.Role, &u.Created)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	u.Created = u.Created.UTC()
	_, _ = s.db.ExecContext(ctx, `UPDATE api_tokens SET last_used = $1 WHERE token_hash = $2`, s.Now(), digest(tok))
	return u, nil
}

// Tokens lists tokens of a user (all users when userID is 0).
func (s *Store) Tokens(ctx context.Context, userID int64) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, user_id, name, created, last_used FROM api_tokens
		WHERE $1 = 0 OR user_id = $2 ORDER BY id`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		var used sql.NullTime
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.Created, &used); err != nil {
			return nil, err
		}
		t.Created, t.LastUsed = t.Created.UTC(), used.Time.UTC()
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteToken revokes a token owned by userID (any owner when userID is 0).
func (s *Store) DeleteToken(ctx context.Context, id, userID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE id = $1 AND ($2::bigint = 0 OR user_id = $3)`, id, userID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
