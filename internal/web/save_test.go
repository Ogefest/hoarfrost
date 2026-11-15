package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func TestBookmarklet(t *testing.T) {
	e := newEnv(t)
	b := e.admin(t)
	jobSource := func(u string) string {
		var src string
		e.db.QueryRowContext(context.Background(), `SELECT source FROM jobs WHERE url = $1`, u).Scan(&src)
		return src
	}

	_, body := b.get(t, "/account")
	m := regexp.MustCompile(`href="javascript:[^"]*/save\?k=([A-Za-z0-9_-]+)`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("account page lacks the bookmarklet:\n%s", body)
	}
	key := m[1]

	// With the key the page is queued at once.
	page := e.site.URL + "/news/article.html"
	resp, body := b.get(t, "/save?"+url.Values{"k": {key}, "url": {page}, "title": {"Radni"}}.Encode())
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Queued for capture") || jobSource(page) != BookmarkletSource {
		t.Fatalf("save with key: %d source %q\n%s", resp.StatusCode, jobSource(page), body)
	}

	// Without a valid key it only asks; the confirmation needs the CSRF token.
	other := e.site.URL + "/news/article2.html"
	for _, q := range []url.Values{{"url": {other}}, {"url": {other}, "k": {"forged"}}} {
		resp, body = b.get(t, "/save?"+q.Encode())
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, `action="/save"`) || jobSource(other) != "" {
			t.Fatalf("save %v must ask first: %d source %q\n%s", q, resp.StatusCode, jobSource(other), body)
		}
	}
	if resp, _ = b.post(t, "/save", url.Values{"url": {other}}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("confirmation without CSRF token: %d", resp.StatusCode)
	}
	resp, body = b.post(t, "/save", url.Values{"url": {other}, "csrf": {b.csrf}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Queued for capture") || jobSource(other) != BookmarkletSource {
		t.Fatalf("confirmed save: %d\n%s", resp.StatusCode, body)
	}
	if resp, body = b.get(t, "/save?url=not-a-url"); resp.StatusCode == http.StatusOK && strings.Contains(body, "Queued") {
		t.Fatal("invalid URL queued")
	}

	// Signed out, the bookmarklet leads through the login back to the page.
	resp, _ = e.browser().get(t, "/save?"+url.Values{"k": {key}, "url": {page}}.Encode())
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "/login?next=") || !strings.Contains(loc, "save") {
		t.Fatalf("anonymous save: %d %s", resp.StatusCode, loc)
	}

	// A new password revokes the key.
	b.get(t, "/account")
	resp, _ = b.post(t, "/account/password", url.Values{"csrf": {b.csrf}, "current": {"admin-password-1"}, "password": {"admin-password-2"}, "password2": {"admin-password-2"}})
	if resp.StatusCode >= 400 {
		t.Fatalf("change password: %d", resp.StatusCode)
	}
	third := e.site.URL + "/news/js.html"
	_, body = b.get(t, "/save?"+url.Values{"k": {key}, "url": {third}}.Encode())
	if !strings.Contains(body, "no longer valid") || jobSource(third) != "" {
		t.Fatalf("old key still accepted:\n%s", body)
	}
}

func TestBookmarkletURL(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://archive.lan:8080/account", nil)
	got := string(bookmarklet(r, "KEY_1-x"))
	if !strings.HasPrefix(got, "javascript:(function(){") || !strings.Contains(got, `"http://archive.lan:8080/save?k=KEY_1-x`) ||
		!strings.Contains(got, "encodeURIComponent(location.href)") {
		t.Fatalf("bookmarklet: %s", got)
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	if got := string(bookmarklet(r, "k")); !strings.Contains(got, `"https://archive.lan:8080/save?k=k`) {
		t.Fatalf("behind TLS proxy: %s", got)
	}
}
