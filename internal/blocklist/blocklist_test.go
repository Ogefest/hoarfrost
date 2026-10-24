package blocklist

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const hostsList = `# StevenBlack style hosts file
127.0.0.1 localhost
127.0.0.1 localhost.localdomain
255.255.255.255 broadcasthost
::1 localhost
0.0.0.0 0.0.0.0
0.0.0.0 ads.example.com tracker.example.net # inline comment
0.0.0.0 Doubleclick.NET
127.0.0.1 not_a..domain
`

const adblockList = `[Adblock Plus 2.0]
! Title: EasyPrivacy-like
||googletagmanager.com^
||stats.example.org^$third-party
||cdn.example.org^$script,domain=news.pl
||example.org/ads/*
@@||allowed.doubleclick.net^
example.com##.banner
*.wildcard-tracker.io
plain-tracker.com
`

func TestParseFormats(t *testing.T) {
	h, err := Parse(strings.NewReader(hostsList))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(h.Exact, ",") != "ads.example.com,tracker.example.net,doubleclick.net" || len(h.Suffix) != 0 {
		t.Fatalf("hosts: %+v", h)
	}
	a, err := Parse(strings.NewReader(adblockList))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(a.Suffix, ",") != "googletagmanager.com,stats.example.org,wildcard-tracker.io" {
		t.Fatalf("adblock suffix: %v", a.Suffix)
	}
	if strings.Join(a.Allow, ",") != "allowed.doubleclick.net" || strings.Join(a.Exact, ",") != "plain-tracker.com" {
		t.Fatalf("adblock: %+v", a)
	}
}

func TestMatching(t *testing.T) {
	h, _ := Parse(strings.NewReader(hostsList))
	a, _ := Parse(strings.NewReader(adblockList))
	s := Compile([]Entries{h, a}, []string{"tracker.example.net"})
	cases := map[string]bool{
		"ads.example.com":          true,
		"sub.ads.example.com":      false, // hosts entries are exact, as in Pi-hole
		"example.com":              false,
		"doubleclick.net":          true,
		"DoubleClick.net.":         true,
		"allowed.doubleclick.net":  false,
		"googletagmanager.com":     true,
		"www.googletagmanager.com": true,
		"x.wildcard-tracker.io":    true,
		"tracker.example.net":      false, // configured allow
		"cdn.example.org":          false, // page-specific rule ignored
		"":                         false,
	}
	for host, want := range cases {
		if got := s.Blocked(host); got != want {
			t.Errorf("Blocked(%q) = %v, want %v", host, got, want)
		}
	}
	if s.Len() != 7 || s.Version() == "" {
		t.Fatalf("len %d version %q", s.Len(), s.Version())
	}
	if Empty().Blocked("ads.example.com") {
		t.Fatal("empty set blocks")
	}
}

func TestEncodeDecode(t *testing.T) {
	a, _ := Parse(strings.NewReader(adblockList + hostsList))
	s := Compile([]Entries{a}, []string{"keep.example"})
	var buf bytes.Buffer
	if err := s.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	d, err := Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if d.Version() != s.Version() || d.Len() != s.Len() || !d.Blocked("www.googletagmanager.com") || d.Blocked("allowed.doubleclick.net") {
		t.Fatal("round trip changed the set")
	}
	if _, err := Decode(strings.NewReader("?x\n")); err == nil {
		t.Fatal("bad line accepted")
	}
}

func TestUpdaterConditionalCacheAndSafety(t *testing.T) {
	var hits, notModified atomic.Int32
	body := hostsList
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/hosts":
			if r.Header.Get("If-None-Match") == `"v1"` && body == hostsList {
				notModified.Add(1)
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"v1"`)
			w.Write([]byte(body))
		case "/adblock":
			w.Write([]byte(adblockList))
		default:
			http.Error(w, "gone", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	urls := []string{srv.URL + "/hosts", srv.URL + "/adblock", srv.URL + "/missing"}
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	u := NewUpdater(urls, nil, dir, time.Hour, nil)
	u.Now = func() time.Time { return now }
	if err := u.Load(); err != nil || u.Set().Len() != 0 || !u.Due() {
		t.Fatal("fresh updater")
	}
	if err := u.Update(context.Background()); err == nil {
		t.Fatal("missing list must be reported")
	}
	if !u.Set().Blocked("ads.example.com") || !u.Set().Blocked("x.googletagmanager.com") || u.Due() {
		t.Fatal("lists not applied")
	}
	st := u.Statuses()
	if st[0].Entries != 3 || st[0].ETag != `"v1"` || st[2].Error == "" {
		t.Fatalf("status %+v", st)
	}
	version := u.Set().Version()

	now = now.Add(2 * time.Hour)
	u.Update(context.Background())
	if notModified.Load() != 1 || u.Set().Version() != version {
		t.Fatal("conditional request not used")
	}

	body = "# broken download\n"
	now = now.Add(2 * time.Hour)
	u.Update(context.Background())
	if !u.Set().Blocked("ads.example.com") || u.Statuses()[0].Error == "" {
		t.Fatal("an empty download must not replace a good list")
	}

	offline := NewUpdater(urls, []string{"ads.example.com"}, dir, time.Hour, nil)
	offline.Load()
	if !offline.Set().Blocked("tracker.example.net") || offline.Set().Blocked("ads.example.com") {
		t.Fatal("cache not loaded or allow ignored")
	}
}

func FuzzParse(f *testing.F) {
	f.Add(hostsList)
	f.Add(adblockList)
	f.Fuzz(func(t *testing.T, s string) {
		e, err := Parse(strings.NewReader(s))
		if err != nil {
			return
		}
		set := Compile([]Entries{e}, nil)
		var buf bytes.Buffer
		set.Encode(&buf)
		d, err := Decode(&buf)
		if err != nil || d.Version() != set.Version() {
			t.Fatalf("round trip: %v", err)
		}
	})
}
