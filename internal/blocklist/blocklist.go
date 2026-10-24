// Package blocklist filters traffic that should not be archived (ads,
// trackers) using public lists in the formats used by Pi-hole and ad
// blockers: hosts files, plain domain lists and adblock "||domain^" rules.
package blocklist

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"regexp"
	"sort"
	"strings"
)

// DefaultURLs are used when the configuration names no lists: the unified
// hosts list that Pi-hole installs by default.
var DefaultURLs = []string{"https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts"}

// Entries are the rules parsed from one list.
type Entries struct {
	// Exact hosts (hosts files, plain domain lists).
	Exact []string
	// Domains blocked with all subdomains (adblock "||domain^", "*.domain").
	Suffix []string
	// Domains never blocked (adblock "@@||domain^").
	Allow []string
}

// Len returns the number of blocking rules.
func (e Entries) Len() int { return len(e.Exact) + len(e.Suffix) }

var domainRe = regexp.MustCompile(`^(?:[a-z0-9_](?:[a-z0-9_-]{0,61}[a-z0-9_])?\.)+[a-z0-9-]{1,63}$`)

// normalize lower-cases a domain and checks its syntax.
func normalize(d string) (string, bool) {
	d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
	if d == "" || net.ParseIP(d) != nil || !domainRe.MatchString(d) {
		return "", false
	}
	return d, true
}

var ignoredHosts = map[string]bool{
	"localhost": true, "localhost.localdomain": true, "local": true, "broadcasthost": true,
	"ip6-localhost": true, "ip6-loopback": true, "ip6-localnet": true, "ip6-mcastprefix": true,
	"ip6-allnodes": true, "ip6-allrouters": true, "ip6-allhosts": true, "0.0.0.0": true,
}

// adblock options that do not restrict a rule to some pages.
var neutralOptions = map[string]bool{"third-party": true, "3p": true, "important": true, "all": true, "": true}

// Parse reads a list in any supported format; formats may be mixed.
// Unsupported rules (paths, wildcards inside names, page-specific options,
// cosmetic filters) are ignored.
func Parse(r io.Reader) (Entries, error) {
	var e Entries
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == '!' || line[0] == '[' || strings.Contains(line, "##") || strings.Contains(line, "#@#") {
			continue
		}
		if i := strings.Index(line, " #"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		switch {
		case strings.HasPrefix(line, "@@||"):
			if d, ok := adblockDomain(line[4:]); ok {
				e.Allow = append(e.Allow, d)
			}
		case strings.HasPrefix(line, "||"):
			if d, ok := adblockDomain(line[2:]); ok {
				e.Suffix = append(e.Suffix, d)
			}
		case strings.HasPrefix(line, "*."):
			if d, ok := normalize(line[2:]); ok {
				e.Suffix = append(e.Suffix, d)
			}
		default:
			fields := strings.Fields(line)
			if len(fields) >= 2 && net.ParseIP(fields[0]) != nil {
				for _, h := range fields[1:] {
					if ignoredHosts[strings.ToLower(h)] {
						continue
					}
					if d, ok := normalize(h); ok {
						e.Exact = append(e.Exact, d)
					}
				}
				continue
			}
			if len(fields) == 1 {
				if d, ok := normalize(fields[0]); ok && !ignoredHosts[d] {
					e.Exact = append(e.Exact, d)
				}
			}
		}
	}
	return e, sc.Err()
}

// adblockDomain extracts the domain of "domain^[$options]" when the rule
// blocks the whole domain on every page.
func adblockDomain(rule string) (string, bool) {
	rule, opts, _ := strings.Cut(rule, "$")
	for _, o := range strings.Split(opts, ",") {
		if !neutralOptions[strings.TrimSpace(o)] {
			return "", false
		}
	}
	rule = strings.TrimSuffix(rule, "|")
	if !strings.HasSuffix(rule, "^") {
		return "", false
	}
	return normalize(strings.TrimSuffix(rule, "^"))
}

// Set is a compiled, immutable set of rules.
type Set struct {
	exact   map[string]struct{}
	suffix  map[string]struct{}
	allow   map[string]struct{}
	version string
}

// Empty returns a set that blocks nothing.
func Empty() *Set { return Compile(nil, nil) }

// Compile merges lists and extra allowed domains into a Set.
func Compile(lists []Entries, allow []string) *Set {
	s := &Set{exact: map[string]struct{}{}, suffix: map[string]struct{}{}, allow: map[string]struct{}{}}
	for _, l := range lists {
		for _, d := range l.Exact {
			s.exact[d] = struct{}{}
		}
		for _, d := range l.Suffix {
			s.suffix[d] = struct{}{}
		}
		for _, d := range l.Allow {
			s.allow[d] = struct{}{}
		}
	}
	for _, a := range allow {
		if d, ok := normalize(a); ok {
			s.allow[d] = struct{}{}
		}
	}
	var buf bytes.Buffer
	_ = s.Encode(&buf)
	sum := sha256.Sum256(buf.Bytes())
	s.version = hex.EncodeToString(sum[:8])
	return s
}

// Version identifies the content of the set.
func (s *Set) Version() string { return s.version }

// Len returns the number of blocking rules.
func (s *Set) Len() int { return len(s.exact) + len(s.suffix) }

// Blocked reports whether a host is blocked. Allowed domains (with their
// subdomains) win over blocking rules.
func (s *Set) Blocked(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" || s.Len() == 0 {
		return false
	}
	if matchSuffix(s.allow, host) {
		return false
	}
	if _, ok := s.exact[host]; ok {
		return true
	}
	return matchSuffix(s.suffix, host)
}

func matchSuffix(set map[string]struct{}, host string) bool {
	if len(set) == 0 {
		return false
	}
	for d := host; ; {
		if _, ok := set[d]; ok {
			return true
		}
		i := strings.IndexByte(d, '.')
		if i < 0 {
			return false
		}
		d = d[i+1:]
	}
}

// Encode writes the set in a line format: "=" exact, "*" suffix, "+" allow.
func (s *Set) Encode(w io.Writer) error {
	bw := bufio.NewWriter(w)
	for _, part := range []struct {
		prefix string
		m      map[string]struct{}
	}{{"=", s.exact}, {"*", s.suffix}, {"+", s.allow}} {
		keys := make([]string, 0, len(part.m))
		for k := range part.m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			bw.WriteString(part.prefix)
			bw.WriteString(k)
			bw.WriteByte('\n')
		}
	}
	return bw.Flush()
}

// Decode reads a set written by Encode.
func Decode(r io.Reader) (*Set, error) {
	var e Entries
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case '=':
			e.Exact = append(e.Exact, line[1:])
		case '*':
			e.Suffix = append(e.Suffix, line[1:])
		case '+':
			e.Allow = append(e.Allow, line[1:])
		default:
			return nil, fmt.Errorf("blocklist: bad line %q", line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return Compile([]Entries{e}, nil), nil
}
