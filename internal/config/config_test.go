package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const example = `
defaults:
  engine: static
  fallback: chrome
  poll: 1h
  max_per_domain: 3
  min_text_chars: 500
  block: [doubleclick.net, googletagmanager.com]

sources:
  - name: serwis-a
    feeds: [https://serwis-a.pl/rss]
  - name: serwis-b
    sitemaps: [https://serwis-b.pl/news-sitemap.xml]
    engine: chrome
    chrome: { scroll: true, wait: 3s }
    max_size: 20MB
    block: [ads.serwis-b.pl]
  - name: pojedyncze
    urls: [https://example.org/raport]
    enabled: false
    poll: 1d
`

func TestParseAndResolve(t *testing.T) {
	s, err := ParseSources([]byte(example))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Sources) != 3 || s.MaxPerDomain() != 3 {
		t.Fatalf("%+v", s)
	}
	a := s.Resolve("serwis-a")
	if a.Engine != "static" || a.Options.Fallback != "chrome" || a.Poll != time.Hour || a.Options.MinTextChars != 500 ||
		!a.Options.Attachments || a.Options.MaxResponseSize != DefaultMaxSize || a.Priority != 100 {
		t.Fatalf("serwis-a %+v", a)
	}
	b := s.Resolve("serwis-b")
	if b.Engine != "chrome" || b.Options.Fallback != "" || !b.Options.ChromeScroll || b.Options.ChromeWait != 3*time.Second ||
		b.Options.MaxResponseSize != 20<<20 || len(b.Options.Block) != 3 {
		t.Fatalf("serwis-b %+v", b)
	}
	c, _ := s.Find("pojedyncze")
	if c.IsEnabled() || s.Resolve("pojedyncze").Poll != 24*time.Hour {
		t.Fatal("pojedyncze")
	}
	api := s.Resolve("api")
	if api.Engine != "static" {
		t.Fatal(api.Engine)
	}
}

func TestBlocklistSettings(t *testing.T) {
	s, _ := ParseSources([]byte("sources: []\n"))
	urls, refresh, _ := s.BlocklistSettings()
	if len(urls) != 1 || urls[0] != DefaultBlocklistURLs[0] || refresh != 24*time.Hour {
		t.Fatalf("defaults %v %v", urls, refresh)
	}
	s, err := ParseSources([]byte("blocklists:\n  urls: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	if urls, _, _ := s.BlocklistSettings(); len(urls) != 0 {
		t.Fatal("empty list must disable block lists")
	}
	s, err = ParseSources([]byte("blocklists:\n  urls: [https://lists.example/hosts]\n  refresh: 6h\n  allow: [cdn.serwis.pl]\n"))
	if err != nil {
		t.Fatal(err)
	}
	urls, refresh, allow := s.BlocklistSettings()
	if urls[0] != "https://lists.example/hosts" || refresh != 6*time.Hour || allow[0] != "cdn.serwis.pl" {
		t.Fatal("explicit settings")
	}
	for _, bad := range []string{"blocklists:\n  urls: [ftp://x]\n", "blocklists:\n  urls: []\n  refresh: 1m\n"} {
		if _, err := ParseSources([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestValidation(t *testing.T) {
	bad := map[string]string{
		"unknown field": "sources:\n  - name: a\n    feedz: [https://a.pl]\n",
		"bad name":      "sources:\n  - name: A B\n    urls: [https://a.pl]\n",
		"duplicate":     "sources:\n  - name: a\n    urls: [https://a.pl]\n  - name: a\n    urls: [https://b.pl]\n",
		"no urls":       "sources:\n  - name: a\n",
		"bad url":       "sources:\n  - name: a\n    urls: [ftp://a.pl]\n",
		"bad engine":    "sources:\n  - name: a\n    engine: lynx\n    urls: [https://a.pl]\n",
		"bad poll":      "defaults:\n  poll: 10s\n",
		"bad size":      "defaults:\n  max_size: lots\n",
		"bad fallback":  "defaults:\n  fallback: x\n",
	}
	for name, y := range bad {
		if _, err := ParseSources([]byte(y)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	if s, err := ParseSources(nil); err != nil || len(s.Sources) != 0 {
		t.Fatalf("empty config: %v", err)
	}
}

func TestRoundTrips(t *testing.T) {
	s, _ := ParseSources([]byte(example))
	y, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	fromYAML, err := ParseSources(y)
	if err != nil {
		t.Fatal(err)
	}
	j, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var fromJSON Sources
	if err := json.Unmarshal(j, &fromJSON); err != nil {
		t.Fatal(err)
	}
	for _, again := range []*Sources{fromYAML, &fromJSON} {
		if again.Resolve("serwis-b").Options.ChromeWait != 3*time.Second || len(again.Sources) != 3 ||
			again.Resolve("serwis-a").Options.MaxResponseSize != s.Resolve("serwis-a").Options.MaxResponseSize {
			t.Fatalf("round trip %+v", again)
		}
	}
	var src Source
	if err := json.Unmarshal([]byte(`{"name":"x","poll":"30m","max_size":"5MB","urls":["https://a.pl/"]}`), &src); err != nil ||
		time.Duration(src.Poll) != 30*time.Minute || src.MaxSize != 5<<20 {
		t.Fatalf("json source %+v %v", src, err)
	}
	if err := json.Unmarshal([]byte(`{"poll":"soon"}`), &src); err == nil {
		t.Fatal("invalid duration accepted")
	}
	if SnapshotKey(time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)) != "config/sources/2026-10-11T120000Z.yaml" {
		t.Fatal("snapshot key")
	}
}

func TestByteSize(t *testing.T) {
	cases := map[string]int64{"1024": 1024, "10KB": 10 << 10, "50MB": 50 << 20, "2GiB": 2 << 30, "1.5 MB": 3 << 19}
	for in, want := range cases {
		if got, err := ParseByteSize(in); err != nil || got != want {
			t.Errorf("%s: %d %v", in, got, err)
		}
	}
}

func TestDotEnv(t *testing.T) {
	in := `# comment
HOARFROST_TEST_A=plain value # trailing
export HOARFROST_TEST_B="quoted \"x\""
HOARFROST_TEST_C='single # not comment'
HOARFROST_TEST_D=
`
	vars, err := ParseDotEnv(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if vars["HOARFROST_TEST_A"] != "plain value" || vars["HOARFROST_TEST_B"] != `quoted "x"` ||
		vars["HOARFROST_TEST_C"] != "single # not comment" || vars["HOARFROST_TEST_D"] != "" {
		t.Fatalf("%v", vars)
	}
	if _, err := ParseDotEnv(strings.NewReader("NOEQUALS")); err == nil {
		t.Fatal("expected error")
	}
	path := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(path, []byte("HOARFROST_TEST_E=fromfile\nHOARFROST_TEST_F=fromfile\n"), 0o600)
	t.Setenv("HOARFROST_TEST_F", "fromenv")
	os.Unsetenv("HOARFROST_TEST_E")
	if err := LoadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Unsetenv("HOARFROST_TEST_E") })
	if os.Getenv("HOARFROST_TEST_E") != "fromfile" || os.Getenv("HOARFROST_TEST_F") != "fromenv" {
		t.Fatal("dotenv must not override the environment")
	}
}

func TestFromEnvironment(t *testing.T) {
	t.Setenv("HOARFROST_DATA_DIR", "/var/lib/hoarfrost")
	t.Setenv("HOARFROST_STORAGE", "")
	t.Setenv("HOARFROST_SEAL_GRACE", "2h")
	t.Setenv("HOARFROST_INCOMING_RETENTION", "14d")
	t.Setenv("HOARFROST_WORKERS", "4")
	e, err := FromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if e.Storage != "file:///var/lib/hoarfrost/archive" || e.SealGrace != 2*time.Hour || e.Retention != 14*24*time.Hour ||
		e.Workers != 4 || e.VolumeSize != 4<<30 {
		t.Fatalf("%+v", e)
	}
	t.Setenv("HOARFROST_VOLUME_SIZE", "2GB")
	if e, err := FromEnvironment(); err != nil || e.VolumeSize != 2<<30 {
		t.Fatalf("volume size %d %v", e.VolumeSize, err)
	}
	for _, bad := range []string{"lots", "6GiB"} {
		t.Setenv("HOARFROST_VOLUME_SIZE", bad)
		if _, err := FromEnvironment(); err == nil {
			t.Fatalf("volume size %s accepted", bad)
		}
	}
	t.Setenv("HOARFROST_VOLUME_SIZE", "")
	if e.RequireSecret() == nil {
		t.Fatal("empty secret accepted")
	}
	if e.Storages["default"] != e.Storage || len(e.StorageNames()) != 1 {
		t.Fatalf("storages %v", e.Storages)
	}
	t.Setenv("HOARFROST_STORAGES", "prasa=s3://bucket/a?endpoint=http://h&region=r, lokalny=file:///srv/lok")
	t.Setenv("HOARFROST_REPLICAS", "prasa=file:///backup/prasa")
	e, err = FromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(e.StorageNames(), ",") != "default,lokalny,prasa" || e.Storages["lokalny"] != "file:///srv/lok" || e.Replicas["prasa"] != "file:///backup/prasa" {
		t.Fatalf("named storages %v %v", e.Storages, e.Replicas)
	}
	for k, v := range map[string]string{"HOARFROST_STORAGES": "Bad Name=file:///x", "HOARFROST_REPLICAS": "nope=file:///x"} {
		t.Setenv(k, v)
		if _, err := FromEnvironment(); err == nil {
			t.Errorf("%s=%s accepted", k, v)
		}
		t.Setenv(k, "")
	}
	t.Setenv("HOARFROST_STORAGES", "default=file:///x")
	if _, err := FromEnvironment(); err == nil {
		t.Error("default in HOARFROST_STORAGES accepted")
	}
	t.Setenv("HOARFROST_STORAGES", "")
	t.Setenv("HOARFROST_WORKERS", "x")
	if _, err := FromEnvironment(); err == nil {
		t.Fatal("bad int accepted")
	}
}
