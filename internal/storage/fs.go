package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const tmpPrefix = ".tmp-"

// FS stores objects as files under a root directory.
type FS struct {
	root string
}

// NewFS returns a filesystem backend rooted at dir, creating it if needed.
func NewFS(dir string) (*FS, error) {
	if dir == "" {
		return nil, errors.New("storage: empty fs root")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	return &FS{root: abs}, nil
}

func (s *FS) String() string { return "file://" + filepath.ToSlash(s.root) }

func (s *FS) path(key string) (string, error) {
	if err := ValidKey(key); err != nil {
		return "", err
	}
	return filepath.Join(s.root, filepath.FromSlash(key)), nil
}

// Put writes to a temporary file, syncs it and renames it into place.
func (s *FS) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, tmpPrefix+"*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, ctxReader{ctx, r})
	if err == nil && size >= 0 && n != size {
		err = fmt.Errorf("storage: put %s: wrote %d bytes, expected %d", key, n, size)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// Create writes to a temporary file next to the object; Commit syncs it and
// renames it into place.
func (s *FS) Create(ctx context.Context, key string, sizeHint int64) (Writer, error) {
	p, err := s.path(key)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, tmpPrefix+"*")
	if err != nil {
		return nil, err
	}
	return &fsWriter{ctx: ctx, f: tmp, path: p, dir: dir}, nil
}

type fsWriter struct {
	ctx   context.Context
	f     *os.File
	path  string
	dir   string
	n     int64
	ended bool
}

func (w *fsWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := w.f.Write(p)
	w.n += int64(n)
	return n, err
}

func (w *fsWriter) Commit() (int64, error) {
	if w.ended {
		return 0, errors.New("storage: writer already finished")
	}
	w.ended = true
	defer os.Remove(w.f.Name())
	err := w.ctx.Err()
	if err == nil {
		err = w.f.Sync()
	}
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(w.f.Name(), w.path)
	}
	if err != nil {
		return w.n, err
	}
	if d, err := os.Open(w.dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return w.n, nil
}

func (w *fsWriter) Abort() {
	if w.ended {
		return
	}
	w.ended = true
	w.f.Close()
	os.Remove(w.f.Name())
}

type fsReader struct {
	*os.File
	size int64
}

func (r fsReader) Size() int64 { return r.size }

func (s *FS) Open(ctx context.Context, key string) (Reader, error) {
	p, err := s.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotExist, key)
		}
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.IsDir() {
		f.Close()
		return nil, fmt.Errorf("%w: %s", ErrNotExist, key)
	}
	return fsReader{f, st.Size()}, nil
}

func (s *FS) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	start := s.root
	if dir := prefix[:strings.LastIndex(prefix, "/")+1]; dir != "" {
		start = filepath.Join(s.root, filepath.FromSlash(dir))
	}
	if !exists(start) {
		return nil, nil
	}
	err := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), tmpPrefix) {
			return nil
		}
		rel, err := filepath.Rel(s.root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, Object{Key: key, Size: info.Size()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, err
}

func (s *FS) Stat(ctx context.Context, key string) (Object, error) {
	p, err := s.path(key)
	if err != nil {
		return Object{}, err
	}
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			return Object{}, fmt.Errorf("%w: %s", ErrNotExist, key)
		}
		return Object{}, err
	}
	return Object{Key: key, Size: st.Size()}, nil
}

func (s *FS) Delete(ctx context.Context, key string) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	s.removeEmptyParents(filepath.Dir(p))
	return nil
}

// removeEmptyParents deletes empty directories up to the root.
func (s *FS) removeEmptyParents(dir string) {
	for ; dir != s.root && strings.HasPrefix(dir, s.root); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			return
		}
	}
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
