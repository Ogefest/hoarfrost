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

// Writer streams a new object whose size is not known in advance. Nothing
// is visible under the key until Commit succeeds; Abort discards the data.
type Writer interface {
	io.Writer
	// Commit stores the object and returns its size.
	Commit() (int64, error)
	// Abort discards the object; it is a no-op after Commit.
	Abort()
}

// Creator is implemented by backends that store an object while it is
// produced, so that objects larger than local disk can be written.
type Creator interface {
	// Create starts a new object under key; sizeHint (-1 when unknown)
	// lets the backend size its buffers for very large objects.
	Create(ctx context.Context, key string, sizeHint int64) (Writer, error)
}

// Streamer is implemented by backends that read a whole object faster
// sequentially than through ReaderAt.
type Streamer interface {
	// Get returns the content of an object and its size.
	Get(ctx context.Context, key string) (io.ReadCloser, int64, error)
}

// Create starts streaming a new object to s. Backends without their own
// implementation receive the data through Put with an unknown size.
func Create(ctx context.Context, s Storage, key string, sizeHint int64) (Writer, error) {
	if c, ok := s.(Creator); ok {
		return c.Create(ctx, key, sizeHint)
	}
	if err := ValidKey(key); err != nil {
		return nil, err
	}
	return newPipeWriter(ctx, func(ctx context.Context, r io.Reader) error { return s.Put(ctx, key, r, -1) }, nil), nil
}

// Stream reads a whole object sequentially.
func Stream(ctx context.Context, s Storage, key string) (io.ReadCloser, int64, error) {
	if st, ok := s.(Streamer); ok {
		return st.Get(ctx, key)
	}
	r, err := s.Open(ctx, key)
	if err != nil {
		return nil, 0, err
	}
	return struct {
		io.Reader
		io.Closer
	}{io.NewSectionReader(r, 0, r.Size()), r}, r.Size(), nil
}

// errAborted ends an upload abandoned by its writer.
var errAborted = errors.New("storage: upload aborted")

// pipeWriter feeds an upload running in the background through a pipe.
type pipeWriter struct {
	pw     *io.PipeWriter
	cancel context.CancelFunc
	done   chan error
	n      int64
	ended  bool
	// check runs after a successful upload, e.g. to compare stored size.
	check func(n int64) error
}

func newPipeWriter(ctx context.Context, upload func(ctx context.Context, r io.Reader) error, check func(n int64) error) *pipeWriter {
	ctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	w := &pipeWriter{pw: pw, cancel: cancel, done: make(chan error, 1), check: check}
	go func() {
		err := upload(ctx, pr)
		if err == nil {
			// The upload must have consumed everything; a writer still
			// blocked in Write would otherwise hang.
			_, err = io.Copy(io.Discard, pr)
		}
		pr.CloseWithError(cmp(err, io.ErrClosedPipe))
		w.done <- err
	}()
	return w
}

// cmp returns err, or fallback when err is nil.
func cmp(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}

func (w *pipeWriter) Write(p []byte) (int, error) {
	n, err := w.pw.Write(p)
	w.n += int64(n)
	return n, err
}

func (w *pipeWriter) Commit() (int64, error) {
	if w.ended {
		return 0, errors.New("storage: writer already finished")
	}
	w.ended = true
	w.pw.Close()
	err := <-w.done
	w.cancel()
	if err == nil && w.check != nil {
		err = w.check(w.n)
	}
	return w.n, err
}

func (w *pipeWriter) Abort() {
	if w.ended {
		return
	}
	w.ended = true
	w.cancel()
	w.pw.CloseWithError(errAborted)
	<-w.done
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
