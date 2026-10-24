package warcio

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"hash"
	"strconv"
	"strings"
	"time"
)

// Record is a single WARC record with its block held in memory.
type Record struct {
	Header Header
	Block  []byte
}

// Type returns WARC-Type.
func (r *Record) Type() string { return r.Header.Get(HType) }

// ID returns WARC-Record-ID.
func (r *Record) ID() string { return r.Header.Get(HRecordID) }

// TargetURI returns WARC-Target-URI.
func (r *Record) TargetURI() string { return r.Header.Get(HTargetURI) }

// Date parses WARC-Date.
func (r *Record) Date() (time.Time, error) { return ParseDate(r.Header.Get(HDate)) }

// New creates a record of the given type with a fresh ID and date.
func New(typ string, date time.Time) *Record {
	r := &Record{}
	r.Header.Set(HType, typ)
	r.Header.Set(HRecordID, NewRecordID())
	r.Header.Set(HDate, FormatDate(date))
	return r
}

// Finalize sets Content-Length and WARC-Block-Digest from the block.
func (r *Record) Finalize() {
	r.Header.Set(HContentLength, strconv.Itoa(len(r.Block)))
	r.Header.Set(HBlockDigest, Digest(r.Block))
}

// NewRecordID returns a random urn:uuid identifier.
func NewRecordID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("<urn:uuid:%x-%x-%x-%x-%x>", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// FormatDate formats a WARC-Date in UTC with second precision.
func FormatDate(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// ParseDate parses a WARC-Date (W3C-ISO8601, optional fractional seconds).
func ParseDate(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("warcio: bad date %q", s)
	}
	return t.UTC(), nil
}

// Digest returns the canonical sha1:BASE32 digest of b.
func Digest(b []byte) string {
	sum := sha1.Sum(b)
	return "sha1:" + base32.StdEncoding.EncodeToString(sum[:])
}

// NewDigester returns a streaming sha1 hasher; format with FormatDigest.
func NewDigester() hash.Hash { return sha1.New() }

// FormatDigest formats a sha1 hash state as sha1:BASE32.
func FormatDigest(h hash.Hash) string {
	return "sha1:" + base32.StdEncoding.EncodeToString(h.Sum(nil))
}

// VerifyDigest reports whether b matches a labelled digest such as
// sha1:BASE32, sha256:hex or md5:hex. Unknown algorithms return an error.
func VerifyDigest(digest string, b []byte) (bool, error) {
	alg, val, ok := strings.Cut(digest, ":")
	if !ok {
		return false, fmt.Errorf("warcio: malformed digest %q", digest)
	}
	var h hash.Hash
	switch strings.ToLower(alg) {
	case "sha1":
		h = sha1.New()
	case "sha256", "sha-256":
		h = sha256.New()
	case "md5":
		h = md5.New()
	default:
		return false, fmt.Errorf("warcio: unsupported digest algorithm %q", alg)
	}
	h.Write(b)
	sum := h.Sum(nil)
	if strings.EqualFold(val, hex.EncodeToString(sum)) {
		return true, nil
	}
	b32 := base32.StdEncoding.EncodeToString(sum)
	return strings.EqualFold(strings.TrimRight(val, "="), strings.TrimRight(b32, "=")), nil
}
