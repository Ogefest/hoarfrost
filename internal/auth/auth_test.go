package auth

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ogefest/hoarfrost/internal/pgdb/pgtest"

	"github.com/ogefest/hoarfrost/internal/appdb"
)

func init() {
	ArgonTime, ArgonMemory, ArgonThreads = 1, 1024, 1
}

func newStore(t *testing.T, secret string) *Store {
	appURL, appSchema := pgtest.Schema(t, "app")
	db, err := appdb.Open(context.Background(), appURL, appSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db, secret)
}

func TestPasswordsUsePepper(t *testing.T) {
	s := newStore(t, "pepper-one-0123456789012345678901")
	h, err := s.HashPassword("correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$") {
		t.Fatal(h)
	}
	if !s.CheckPassword(h, "correct horse") || s.CheckPassword(h, "wrong horse") {
		t.Fatal("check")
	}
	other := New(nil, "pepper-two-0123456789012345678901")
	if other.CheckPassword(h, "correct horse") {
		t.Fatal("hash must depend on the pepper")
	}
	h2, _ := s.HashPassword("correct horse")
	if h == h2 {
		t.Fatal("salt not random")
	}
	for _, bad := range []string{"", "$argon2id$x", "$bcrypt$a$b$c$d", "$argon2id$v=19$m=x$a$b"} {
		if s.CheckPassword(bad, "x") {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestUsersAndAuthenticate(t *testing.T) {
	s := newStore(t, "secret-0123456789012345678901234")
	ctx := context.Background()
	u, err := s.CreateUser(ctx, "anna", "dlugie-haslo-1", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "anna", "dlugie-haslo-1", RoleAdmin); err == nil {
		t.Fatal("duplicate login")
	}
	for _, c := range [][3]string{{"a b", "dlugie-haslo-1", RoleViewer}, {"bob", "shrt", RoleViewer}, {"bob", "dlugie-haslo-1", "root"}} {
		if _, err := s.CreateUser(ctx, c[0], c[1], c[2]); err == nil {
			t.Errorf("accepted %v", c)
		}
	}
	got, err := s.Authenticate(ctx, "anna", "dlugie-haslo-1")
	if err != nil || got.ID != u.ID || got.Role != RoleAdmin {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "anna", "zle-haslo-xyz"); err != ErrInvalidCredentials {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, "nobody", "dlugie-haslo-1"); err != ErrInvalidCredentials {
		t.Fatal(err)
	}

	sess, _ := s.CreateSession(ctx, u, time.Hour)
	if err := s.SetPassword(ctx, "anna", "inne-haslo-123"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, sess.ID); err != ErrNotFound {
		t.Fatal("password reset must end sessions")
	}
	if _, err := s.Authenticate(ctx, "anna", "inne-haslo-123"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword(ctx, "ghost", "inne-haslo-123"); err != ErrNotFound {
		t.Fatal(err)
	}
	if err := s.SetRole(ctx, "anna", RoleViewer); err != nil {
		t.Fatal(err)
	}
	users, _ := s.Users(ctx)
	if len(users) != 1 || users[0].Role != RoleViewer {
		t.Fatal(users)
	}
	if n, _ := s.CountUsers(ctx); n != 1 {
		t.Fatal(n)
	}
	if err := s.DeleteUser(ctx, "anna"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, "anna"); err != ErrNotFound {
		t.Fatal(err)
	}
}

func TestSessions(t *testing.T) {
	s := newStore(t, "secret-0123456789012345678901234")
	ctx := context.Background()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	u, _ := s.CreateUser(ctx, "jan", "dlugie-haslo-1", RoleEditor)
	sess, err := s.CreateSession(ctx, u, time.Hour)
	if err != nil || len(sess.ID) < 40 || sess.CSRF == "" {
		t.Fatal(err)
	}
	got, err := s.Session(ctx, sess.ID)
	if err != nil || got.User.Login != "jan" || got.CSRF != sess.CSRF {
		t.Fatal(err)
	}
	var stored string
	s.db.QueryRow(`SELECT id FROM sessions`).Scan(&stored)
	if stored == sess.ID {
		t.Fatal("session id must be stored hashed")
	}
	now = now.Add(2 * time.Hour)
	if _, err := s.Session(ctx, sess.ID); err != ErrNotFound {
		t.Fatal("expired session accepted")
	}
	sess2, _ := s.CreateSession(ctx, u, time.Hour)
	s.DeleteSession(ctx, sess2.ID)
	if _, err := s.Session(ctx, sess2.ID); err != ErrNotFound {
		t.Fatal("deleted session accepted")
	}
	if err := s.PurgeSessions(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestTokens(t *testing.T) {
	s := newStore(t, "secret-0123456789012345678901234")
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "ext", "dlugie-haslo-1", RoleEditor)
	tok, err := s.CreateToken(ctx, u, "browser extension")
	if err != nil || !strings.HasPrefix(tok, TokenPrefix) {
		t.Fatal(err)
	}
	got, err := s.TokenUser(ctx, tok)
	if err != nil || got.ID != u.ID {
		t.Fatal(err)
	}
	if _, err := s.TokenUser(ctx, tok+"x"); err != ErrNotFound {
		t.Fatal("wrong token accepted")
	}
	if _, err := s.TokenUser(ctx, "nope"); err != ErrNotFound {
		t.Fatal("non-prefixed token accepted")
	}
	toks, _ := s.Tokens(ctx, u.ID)
	if len(toks) != 1 || toks[0].LastUsed.IsZero() {
		t.Fatalf("%+v", toks)
	}
	if err := s.DeleteToken(ctx, toks[0].ID, u.ID+1); err != ErrNotFound {
		t.Fatal("deleted another user's token")
	}
	if err := s.DeleteToken(ctx, toks[0].ID, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TokenUser(ctx, tok); err != ErrNotFound {
		t.Fatal("revoked token accepted")
	}
}

func TestCan(t *testing.T) {
	if !Can(RoleAdmin, RoleEditor) || Can(RoleViewer, RoleEditor) || Can("x", RoleViewer) || Can(RoleAdmin, "x") {
		t.Fatal("Can")
	}
}

func TestAPIKeys(t *testing.T) {
	s := newStore(t, "secret-0123456789012345678901234")
	ctx := context.Background()
	if _, err := s.CreateAPIKey(ctx, " ", "admin", time.Time{}); err == nil {
		t.Fatal("key without a name")
	}
	key, err := s.CreateAPIKey(ctx, "dashboard", "admin", time.Time{})
	if err != nil || !strings.HasPrefix(key, APIKeyPrefix) {
		t.Fatal(err)
	}
	k, err := s.CheckAPIKey(ctx, key)
	if err != nil || k.Name != "dashboard" || k.CreatedBy != "admin" || !k.Expires.IsZero() {
		t.Fatalf("%+v %v", k, err)
	}
	if _, err := s.CheckAPIKey(ctx, key+"x"); err != ErrNotFound {
		t.Fatal("wrong key accepted")
	}
	if _, err := s.TokenUser(ctx, key); err != ErrNotFound {
		t.Fatal("API key accepted as a user token")
	}
	u, _ := s.CreateUser(ctx, "ext", "dlugie-haslo-1", RoleViewer)
	tok, _ := s.CreateToken(ctx, u, "t")
	if _, err := s.CheckAPIKey(ctx, tok); err != ErrNotFound {
		t.Fatal("user token accepted as an API key")
	}

	now := time.Now()
	short, _ := s.CreateAPIKey(ctx, "short", "admin", now.Add(time.Hour))
	if _, err := s.CheckAPIKey(ctx, short); err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, err := s.CheckAPIKey(ctx, short); err != ErrNotFound {
		t.Fatal("expired key accepted")
	}
	keys, _ := s.APIKeys(ctx)
	if len(keys) != 2 || keys[0].Name != "short" || !keys[0].Expired(s.Now()) || keys[1].LastUsed.IsZero() {
		t.Fatalf("%+v", keys)
	}
	if err := s.DeleteAPIKey(ctx, keys[1].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAPIKey(ctx, keys[1].ID); err != ErrNotFound {
		t.Fatal("deleted twice")
	}
	if _, err := s.CheckAPIKey(ctx, key); err != ErrNotFound {
		t.Fatal("revoked key accepted")
	}
}
