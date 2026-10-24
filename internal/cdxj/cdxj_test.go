package cdxj

import (
	"bytes"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestSURT(t *testing.T) {
	cases := map[string]string{
		"https://www.Example.com/":             "com,example)/",
		"http://example.com":                   "com,example)/",
		"https://serwis.pl/style.css":          "pl,serwis)/style.css",
		"https://www2.serwis.pl/A/B/":          "pl,serwis)/a/b",
		"http://example.com:80/x":              "com,example)/x",
		"https://example.com:443/x":            "com,example)/x",
		"https://example.com:8443/x":           "com,example:8443)/x",
		"https://example.com/p?b=2&a=1#frag":   "com,example)/p?a=1&b=2",
		"http://192.168.1.1/x":                 "1,1,168,192)/x",
		"https://sub.news.example.co.uk/a%20b": "uk,co,example,news,sub)/a%20b",
		"https://example.com./x":               "com,example)/x",
		"urn:something":                        "urn:something",
	}
	for in, want := range cases {
		if got := SURT(in); got != want {
			t.Errorf("SURT(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLineRoundTrip(t *testing.T) {
	l := Line{
		URL:       "https://example.com/a",
		Timestamp: time.Date(2026, 10, 11, 7, 2, 3, 0, time.UTC),
		Mime:      "text/html",
		Status:    "200",
		Digest:    "sha1:ABC",
		Length:    "123",
		Offset:    "456",
		Filename:  "a.warc.gz",
		RecordID:  "<urn:uuid:1>",
		Type:      "response",
	}
	s := l.String()
	if !strings.HasPrefix(s, "com,example)/a 20261011070203 {\"url\":") {
		t.Fatalf("bad line %s", s)
	}
	got, err := Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	l.SURT = "com,example)/a"
	if got != l {
		t.Fatalf("round trip mismatch:\n%+v\n%+v", got, l)
	}
}

func TestParseErrors(t *testing.T) {
	for _, s := range []string{"", "a", "a b", "a 2026x {}", "a 2026 {", "a 123 {}"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) expected error", s)
		}
	}
	if ts, err := ParseTimestamp("2026"); err != nil || ts.Year() != 2026 || ts.Month() != 1 {
		t.Fatal(err)
	}
}

func TestSortMatchesCSort(t *testing.T) {
	if _, err := exec.LookPath("sort"); err != nil {
		t.Skip("sort not available")
	}
	rng := rand.New(rand.NewSource(1))
	alphabet := []string{"a", "B", ",", ")", "/", "?", "=", "&", "-", "_", "0", "9", "~", "%2F", "ą", " "}
	var lines []string
	for range 500 {
		var b strings.Builder
		for range 1 + rng.Intn(12) {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		lines = append(lines, b.String())
	}
	cmd := exec.Command("sort")
	cmd.Env = []string{"LC_ALL=C"}
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	Sort(lines)
	if got := strings.Join(lines, "\n") + "\n"; got != string(out) {
		t.Fatal("Sort differs from LC_ALL=C sort")
	}
}

func TestWriteReadSorted(t *testing.T) {
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	entries := []Line{
		{URL: "https://b.com/", Timestamp: ts, Length: "1", Offset: "0", Filename: "f"},
		{URL: "https://a.com/z", Timestamp: ts, Length: "1", Offset: "1", Filename: "f"},
		{URL: "https://a.com/a", Timestamp: ts.Add(time.Hour), Length: "1", Offset: "2", Filename: "f"},
		{URL: "https://a.com/a", Timestamp: ts, Length: "1", Offset: "3", Filename: "f"},
	}
	var buf bytes.Buffer
	if err := Write(&buf, entries); err != nil {
		t.Fatal(err)
	}
	ok, err := IsSorted(bytes.NewReader(buf.Bytes()))
	if err != nil || !ok {
		t.Fatal("output not sorted")
	}
	got, err := ReadAll(&buf)
	if err != nil || len(got) != 4 {
		t.Fatal(err)
	}
	if got[0].Offset != "3" || got[3].URL != "https://b.com/" {
		t.Fatalf("unexpected order: %+v", got)
	}
	if ok, _ := IsSorted(strings.NewReader("b\na\n")); ok {
		t.Fatal("IsSorted accepted unsorted input")
	}
}

func FuzzParse(f *testing.F) {
	f.Add(`com,example)/ 20261011070203 {"url":"https://example.com/","length":"1","offset":"0","filename":"f"}`)
	f.Fuzz(func(t *testing.T, s string) {
		l, err := Parse(s)
		if err != nil {
			return
		}
		if _, err := Parse(l.String()); err != nil {
			t.Fatalf("re-parse failed: %v", err)
		}
	})
}

func FuzzSURT(f *testing.F) {
	f.Add("https://www.example.com:443/A?b=1&a=2")
	f.Fuzz(func(t *testing.T, s string) {
		SURT(s)
	})
}

// TestSURTMatchesPython compares SURT keys with the Python surt library used
// by pywb, warcio and py-wacz, so CDXJ lookups by those tools match.
func TestSURTMatchesPython(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err == nil {
		err = exec.Command(py, "-c", "import surt").Run()
	}
	if err != nil {
		if os.Getenv("HOARFROST_CONFORMANCE") == "required" {
			t.Fatal("python3 with the surt package is required")
		}
		t.Skip("python surt not available")
	}
	urls := []string{
		"https://www.Example.com/", "http://example.com", "https://serwis.pl/style.css",
		"https://www2.serwis.pl/A/B/", "http://example.com:80/x", "https://example.com:443/x",
		"https://example.com:8443/x", "https://example.com/p?b=2&a=1#frag", "http://192.168.1.1/x",
		"http://127.0.0.1:8080/news/a.html", "https://sub.news.example.co.uk/a%20b",
		"https://serwis.pl/artykul,123.html?utm_source=rss", "https://serwis.pl/a/b.html?x=1&x=0&A=2",
		"https://SERWIS.pl/Kraj/Artykul.HTML", "https://serwis.pl/szukaj?q=budżet",
		"https://serwis.pl/", "https://serwis.pl/path/", "https://cdn.serwis.pl/img/a.jpg?w=800&h=600",
		"https://serwis.pl/żółć/gęś.html", "https://serwis.pl/a%2Fb?x=%C5%BC", "https://serwis.pl/a?q=a b",
		"https://www.serwis.pl:443/", "http://WWW3.Example.ORG/Index.php?Session=1", "https://serwis.pl/?",
		"https://xn--bdet-cva.pl/", "https://serwis.pl/a;b?c=d",
		"https://a.pl/a//b/./c/../d", "https://a.pl/%41%25%2541", "https://a.pl/a%23b?x=%26y&z=%3D",
		"https://a.pl/%7Euser", "https://a.pl/a?b=c%20d", "https://a.pl/x/..", "https://a.pl//double/",
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	pieces := []string{"a", "B", "/", "//", ".", "..", "%2F", "%41", "%25", "ż", " ", "-", "_", "~", ";", ",", "=", "&", "?", "%C5%BC", "x1"}
	hosts := []string{"serwis.pl", "www.Serwis.PL", "news.example.co.uk", "10.0.0.1", "example.com:8080"}
	for range 2000 {
		var b strings.Builder
		b.WriteString("https://" + hosts[rng.Intn(len(hosts))] + "/")
		for range 1 + rng.Intn(8) {
			b.WriteString(pieces[rng.Intn(len(pieces))])
		}
		urls = append(urls, b.String())
	}
	script := "import surt,sys\nfor u in sys.stdin.read().split('\\n')[:-1]:\n    print(surt.surt(u))"
	cmd := exec.Command(py, "-c", script)
	cmd.Stdin = strings.NewReader(strings.Join(urls, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(want) != len(urls) {
		t.Fatalf("python returned %d keys for %d URLs", len(want), len(urls))
	}
	for i, u := range urls {
		if got := SURT(u); got != want[i] {
			t.Errorf("SURT(%q) = %q, python surt %q", u, got, want[i])
		}
	}
}
