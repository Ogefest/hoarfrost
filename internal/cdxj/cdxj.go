package cdxj

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Line is one CDXJ entry. Fields after Filename are extensions specific to
// hoarfrost; other tools ignore them.
type Line struct {
	SURT      string    `json:"-"`
	Timestamp time.Time `json:"-"`

	URL      string `json:"url"`
	Mime     string `json:"mime,omitempty"`
	Status   string `json:"status,omitempty"`
	Digest   string `json:"digest,omitempty"`
	Length   string `json:"length"`
	Offset   string `json:"offset"`
	Filename string `json:"filename"`

	RecordID string `json:"rid,omitempty"`
	Type     string `json:"type,omitempty"`
	RefersTo string `json:"refersTo,omitempty"`
}

// TimestampFormat is the 14-digit CDX timestamp layout.
const TimestampFormat = "20060102150405"

// String formats the line without a trailing newline.
func (l Line) String() string {
	js, _ := json.Marshal(l)
	surt := l.SURT
	if surt == "" {
		surt = SURT(l.URL)
	}
	surt = strings.ReplaceAll(surt, " ", "%20")
	if surt == "" {
		surt = "-"
	}
	return surt + " " + l.Timestamp.UTC().Format(TimestampFormat) + " " + string(js)
}

// Parse parses a single CDXJ line.
func Parse(s string) (Line, error) {
	var l Line
	s = strings.TrimRight(s, "\r\n")
	key, rest, ok := strings.Cut(s, " ")
	if !ok {
		return l, fmt.Errorf("cdxj: malformed line")
	}
	ts, js, ok := strings.Cut(rest, " ")
	if !ok {
		return l, fmt.Errorf("cdxj: malformed line")
	}
	t, err := ParseTimestamp(ts)
	if err != nil {
		return l, err
	}
	if err := json.Unmarshal([]byte(js), &l); err != nil {
		return l, fmt.Errorf("cdxj: %w", err)
	}
	l.SURT = key
	l.Timestamp = t
	return l, nil
}

// ParseTimestamp parses a CDX timestamp of 4 to 14 digits (missing parts are zero).
func ParseTimestamp(ts string) (time.Time, error) {
	if len(ts) < 4 || len(ts) > 14 {
		return time.Time{}, fmt.Errorf("cdxj: bad timestamp %q", ts)
	}
	for _, c := range ts {
		if c < '0' || c > '9' {
			return time.Time{}, fmt.Errorf("cdxj: bad timestamp %q", ts)
		}
	}
	padded := ts + "00000101000000"[len(ts):]
	t, err := time.Parse(TimestampFormat, padded)
	if err != nil {
		return time.Time{}, fmt.Errorf("cdxj: bad timestamp %q", ts)
	}
	return t, nil
}

// Sort orders lines bytewise, matching `LC_ALL=C sort`.
func Sort(lines []string) {
	sort.Strings(lines)
}

// Write writes lines sorted bytewise, one per line.
func Write(w io.Writer, entries []Line) error {
	lines := make([]string, len(entries))
	for i, e := range entries {
		lines[i] = e.String()
	}
	Sort(lines)
	bw := bufio.NewWriter(w)
	for _, l := range lines {
		bw.WriteString(l)
		bw.WriteByte('\n')
	}
	return bw.Flush()
}

// ReadAll parses all non-empty lines from r.
func ReadAll(r io.Reader) ([]Line, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var out []Line
	for n := 1; sc.Scan(); n++ {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		l, err := Parse(sc.Text())
		if err != nil {
			return out, fmt.Errorf("line %d: %w", n, err)
		}
		out = append(out, l)
	}
	return out, sc.Err()
}

// IsSorted reports whether lines read from r are in bytewise order.
func IsSorted(r io.Reader) (bool, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var prev []byte
	first := true
	for sc.Scan() {
		cur := sc.Bytes()
		if !first && bytes.Compare(prev, cur) > 0 {
			return false, nil
		}
		prev = append(prev[:0], cur...)
		first = false
	}
	return true, sc.Err()
}
