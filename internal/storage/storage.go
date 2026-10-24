// Package storage abstracts the archive object store. Objects are immutable:
// they are written once with Put and only ever deleted by cleanup.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strings"
)

// ErrNotExist is returned when an object does not exist.
var ErrNotExist = errors.New("storage: object does not exist")

// Object describes a stored object.
type Object struct {
	Key    string
	Size   int64
	SHA256 string // empty when the backend does not know it
}

// Reader gives random access to an object.
type Reader interface {
	io.ReaderAt
	io.Closer
	Size() int64
}

// Storage is an object store holding immutable archive files.
type Storage interface {
	// Put atomically stores size bytes from r under key.
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	// Open returns random access to an object.
	Open(ctx context.Context, key string) (Reader, error)
	// List returns objects whose keys start with prefix, sorted by key.
	List(ctx context.Context, prefix string) ([]Object, error)
	// Stat returns object metadata or ErrNotExist.
	Stat(ctx context.Context, key string) (Object, error)
	// Delete removes an object; deleting a missing object is not an error.
	Delete(ctx context.Context, key string) error
	// String describes the backend without secrets.
	String() string
}

// ValidKey reports whether key is a clean relative slash-separated path.
func ValidKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") ||
		path.Clean(key) != key || strings.Contains(key, "\\") || key == "." ||
		strings.HasPrefix(key, "../") || key == ".." {
		return fmt.Errorf("storage: invalid key %q", key)
	}
	for _, part := range strings.Split(key, "/") {
		if strings.HasPrefix(part, tmpPrefix) {
			return fmt.Errorf("storage: invalid key %q", key)
		}
	}
	return nil
}

// Open creates a Storage from a URL: file:///path or
// s3://bucket/prefix?endpoint=https://host&region=r.
func Open(ctx context.Context, rawURL string) (Storage, error) {
	return OpenNamed(ctx, "", rawURL)
}

// OpenNamed opens the storage called name. S3 credentials are taken from
// HOARFROST_S3_<NAME>_ACCESS_KEY_ID and HOARFROST_S3_<NAME>_SECRET_ACCESS_KEY when
// set, otherwise from AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY.
func OpenNamed(ctx context.Context, name, rawURL string) (Storage, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	switch u.Scheme {
	case "file", "":
		p := u.Path
		if u.Scheme == "" {
			p = rawURL
		}
		if u.Host != "" {
			return nil, fmt.Errorf("storage: file URL must not have a host: %s", rawURL)
		}
		return NewFS(p)
	case "s3":
		access, secret := os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY")
		if name != "" {
			prefix := "HOARFROST_S3_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
			if k := os.Getenv(prefix + "_ACCESS_KEY_ID"); k != "" {
				access, secret = k, os.Getenv(prefix+"_SECRET_ACCESS_KEY")
			}
		}
		return NewS3FromURL(ctx, u, access, secret)
	default:
		return nil, fmt.Errorf("storage: unsupported scheme %q", u.Scheme)
	}
}

// ReadAll reads a whole object into memory.
func ReadAll(ctx context.Context, s Storage, key string) ([]byte, error) {
	r, err := s.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	buf := make([]byte, r.Size())
	if _, err := r.ReadAt(buf, 0); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf, nil
}

// PutBytes stores b under key.
func PutBytes(ctx context.Context, s Storage, key string, b []byte) error {
	return s.Put(ctx, key, strings.NewReader(string(b)), int64(len(b)))
}
