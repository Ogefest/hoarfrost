// Package storagetest contains the contract test suite every storage backend
// must pass.
package storagetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strconv"
	"testing"

	"github.com/ogefest/hoarfrost/internal/storage"
)

// Run executes the contract suite. newStorage must return an empty backend.
func Run(t *testing.T, newStorage func(t *testing.T) storage.Storage) {
	t.Run("PutOpenReadAt", func(t *testing.T) { testPutOpen(t, newStorage(t)) })
	t.Run("List", func(t *testing.T) { testList(t, newStorage(t)) })
	t.Run("StatDelete", func(t *testing.T) { testStatDelete(t, newStorage(t)) })
	t.Run("AtomicPut", func(t *testing.T) { testAtomic(t, newStorage(t)) })
	t.Run("InvalidKeys", func(t *testing.T) { testInvalidKeys(t, newStorage(t)) })
	t.Run("Large", func(t *testing.T) { testLarge(t, newStorage(t)) })
	t.Run("CreateStream", func(t *testing.T) { testCreateStream(t, newStorage(t)) })
	t.Run("CreateAbort", func(t *testing.T) { testCreateAbort(t, newStorage(t)) })
}

// testCreateStream writes an object in small pieces without knowing its size
// (spanning several upload parts) and reads it back sequentially.
func testCreateStream(t *testing.T, s storage.Storage) {
	ctx := context.Background()
	rng := rand.New(rand.NewSource(7))
	data := randomBytes(rng, 37<<20+11)
	w, err := storage.Create(ctx, s, "weekly/2026/W41.wacz", -1)
	if err != nil {
		t.Fatal(err)
	}
	for rest := data; len(rest) > 0; {
		n := min(len(rest), rng.Intn(1<<20)+1)
		if _, err := w.Write(rest[:n]); err != nil {
			t.Fatal(err)
		}
		rest = rest[n:]
		if len(rest) > len(data)/2 {
			if _, err := s.Stat(ctx, "weekly/2026/W41.wacz"); !errors.Is(err, storage.ErrNotExist) {
				t.Fatalf("object visible before commit: %v", err)
			}
		}
	}
	n, err := w.Commit()
	if err != nil || n != int64(len(data)) {
		t.Fatalf("commit %d %v", n, err)
	}
	if o, err := s.Stat(ctx, "weekly/2026/W41.wacz"); err != nil || o.Size != int64(len(data)) {
		t.Fatalf("stat %+v %v", o, err)
	}
	rc, size, err := storage.Stream(ctx, s, "weekly/2026/W41.wacz")
	if err != nil || size != int64(len(data)) {
		t.Fatalf("stream %d %v", size, err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("streamed content differs (%d bytes): %v", len(got), err)
	}
	if _, _, err := storage.Stream(ctx, s, "missing/key"); !errors.Is(err, storage.ErrNotExist) {
		t.Fatalf("Stream missing: %v", err)
	}
}

func testCreateAbort(t *testing.T, s storage.Storage) {
	ctx := context.Background()
	w, err := storage.Create(ctx, s, "weekly/2026/W41.wacz", 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(bytes.Repeat([]byte("x"), 20<<20)); err != nil {
		t.Fatal(err)
	}
	w.Abort()
	w.Abort()
	if _, err := w.Commit(); err == nil {
		t.Fatal("commit after abort succeeded")
	}
	if _, err := s.Stat(ctx, "weekly/2026/W41.wacz"); !errors.Is(err, storage.ErrNotExist) {
		t.Fatalf("aborted object visible: %v", err)
	}
	objs, err := s.List(ctx, "")
	if err != nil || len(objs) != 0 {
		t.Fatalf("leftover objects: %+v %v", objs, err)
	}

	cctx, cancel := context.WithCancel(ctx)
	w, err = storage.Create(cctx, s, "weekly/2026/W42.wacz", -1)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("partial"))
	cancel()
	if _, err := w.Commit(); err == nil {
		t.Fatal("commit with a cancelled context succeeded")
	}
	if _, err := s.Stat(ctx, "weekly/2026/W42.wacz"); !errors.Is(err, storage.ErrNotExist) {
		t.Fatalf("cancelled object visible: %v", err)
	}
}

func randomBytes(rng *rand.Rand, n int) []byte {
	b := make([]byte, n)
	rng.Read(b)
	return b
}

func testPutOpen(t *testing.T, s storage.Storage) {
	ctx := context.Background()
	rng := rand.New(rand.NewSource(42))
	data := randomBytes(rng, 3<<20+123)
	if err := s.Put(ctx, "weekly/2026/W41.wacz", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	r, err := s.Open(ctx, "weekly/2026/W41.wacz")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Size() != int64(len(data)) {
		t.Fatalf("size %d", r.Size())
	}
	for range 200 {
		off := rng.Int63n(int64(len(data)))
		n := rng.Intn(1<<21) + 1
		buf := make([]byte, n)
		got, err := r.ReadAt(buf, off)
		end := min(off+int64(n), int64(len(data)))
		if int64(got) != end-off {
			t.Fatalf("ReadAt(%d,%d) = %d, %v", off, n, got, err)
		}
		if end-off < int64(n) && !errors.Is(err, io.EOF) {
			t.Fatalf("short read without EOF at %d", off)
		}
		if end-off == int64(n) && err != nil {
			t.Fatalf("ReadAt(%d,%d): %v", off, n, err)
		}
		if !bytes.Equal(buf[:got], data[off:end]) {
			t.Fatalf("ReadAt(%d,%d) content mismatch", off, n)
		}
	}
	tail := make([]byte, 100)
	if n, err := r.ReadAt(tail, int64(len(data))-100); n != 100 || err != nil {
		t.Fatalf("tail read %d %v", n, err)
	}
	if !bytes.Equal(tail, data[len(data)-100:]) {
		t.Fatal("tail mismatch")
	}
	if _, err := r.ReadAt(tail, int64(len(data))); !errors.Is(err, io.EOF) {
		t.Fatalf("read past end: %v", err)
	}

	if _, err := s.Open(ctx, "missing/key"); !errors.Is(err, storage.ErrNotExist) {
		t.Fatalf("Open missing: %v", err)
	}
}

func testList(t *testing.T, s storage.Storage) {
	ctx := context.Background()
	keys := []string{
		"incoming/2026/W41/b.warc.gz",
		"incoming/2026/W41/a.warc.gz",
		"incoming/2026/W42/c.warc.gz",
		"incoming/2026/W4.txt",
		"weekly/2026/W41.wacz",
		"README.md",
	}
	for i, k := range keys {
		if err := storage.PutBytes(ctx, s, k, []byte(strconv.Itoa(i))); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string][]string{
		"incoming/2026/W41/": {"incoming/2026/W41/a.warc.gz", "incoming/2026/W41/b.warc.gz"},
		"incoming/2026/W4":   {"incoming/2026/W4.txt", "incoming/2026/W41/a.warc.gz", "incoming/2026/W41/b.warc.gz", "incoming/2026/W42/c.warc.gz"},
		"weekly/":            {"weekly/2026/W41.wacz"},
		"nothing/":           nil,
		"":                   {"README.md", "incoming/2026/W4.txt", "incoming/2026/W41/a.warc.gz", "incoming/2026/W41/b.warc.gz", "incoming/2026/W42/c.warc.gz", "weekly/2026/W41.wacz"},
	}
	for prefix, want := range cases {
		objs, err := s.List(ctx, prefix)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, o := range objs {
			got = append(got, o.Key)
			if o.Size != 1 {
				t.Errorf("%s size %d", o.Key, o.Size)
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("List(%q) = %v, want %v", prefix, got, want)
		}
	}
}

func testStatDelete(t *testing.T, s storage.Storage) {
	ctx := context.Background()
	if err := storage.PutBytes(ctx, s, "incoming/2026/W41/x.warc.gz", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	o, err := s.Stat(ctx, "incoming/2026/W41/x.warc.gz")
	if err != nil || o.Size != 5 || o.Key != "incoming/2026/W41/x.warc.gz" {
		t.Fatalf("stat %+v %v", o, err)
	}
	if err := s.Delete(ctx, "incoming/2026/W41/x.warc.gz"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(ctx, "incoming/2026/W41/x.warc.gz"); !errors.Is(err, storage.ErrNotExist) {
		t.Fatalf("stat after delete: %v", err)
	}
	if err := s.Delete(ctx, "incoming/2026/W41/x.warc.gz"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
	if _, err := s.Stat(ctx, "incoming"); !errors.Is(err, storage.ErrNotExist) {
		t.Fatalf("stat prefix: %v", err)
	}
}

type failingReader struct {
	data []byte
	pos  int
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.pos >= len(f.data)/2 {
		return 0, errors.New("connection reset")
	}
	n := copy(p, f.data[f.pos:len(f.data)/2])
	f.pos += n
	return n, nil
}

func testAtomic(t *testing.T, s storage.Storage) {
	ctx := context.Background()
	data := bytes.Repeat([]byte("x"), 1<<20)
	err := s.Put(ctx, "incoming/2026/W41/partial.warc.gz", &failingReader{data: data}, int64(len(data)))
	if err == nil {
		t.Fatal("expected error from failing reader")
	}
	if _, err := s.Stat(ctx, "incoming/2026/W41/partial.warc.gz"); !errors.Is(err, storage.ErrNotExist) {
		t.Fatalf("partial object visible: %v", err)
	}
	objs, err := s.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 0 {
		t.Fatalf("leftover objects: %+v", objs)
	}

	err = s.Put(ctx, "short", bytes.NewReader([]byte("abc")), 10)
	if err == nil {
		t.Fatal("expected error for size mismatch")
	}
	if _, err := s.Stat(ctx, "short"); !errors.Is(err, storage.ErrNotExist) {
		t.Fatalf("short object visible: %v", err)
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.Put(cctx, "cancelled", bytes.NewReader(data), int64(len(data))); err == nil {
		t.Fatal("expected error for cancelled context")
	}
	if _, err := s.Stat(ctx, "cancelled"); !errors.Is(err, storage.ErrNotExist) {
		t.Fatalf("cancelled object visible: %v", err)
	}
}

func testInvalidKeys(t *testing.T, s storage.Storage) {
	ctx := context.Background()
	for _, k := range []string{"", "/abs", "a/../b", "../x", "dir/", "a//b", ".tmp-x", "a/.tmp-y"} {
		if err := storage.PutBytes(ctx, s, k, []byte("x")); err == nil {
			t.Errorf("Put(%q) accepted invalid key", k)
		}
	}
}

// zeroReader produces n zero bytes without allocating them.
type zeroReader struct{ n int64 }

func (z *zeroReader) Read(p []byte) (int, error) {
	if z.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > z.n {
		p = p[:z.n]
	}
	clear(p)
	z.n -= int64(len(p))
	return len(p), nil
}

func testLarge(t *testing.T, s storage.Storage) {
	size, _ := strconv.ParseInt(os.Getenv("HOARFROST_TEST_LARGE"), 10, 64)
	if size <= 0 {
		t.Skip("set HOARFROST_TEST_LARGE=<bytes> (e.g. 5500000000) to test large objects")
	}
	ctx := context.Background()
	if err := s.Put(ctx, "large.bin", &zeroReader{size}, size); err != nil {
		t.Fatal(err)
	}
	r, err := s.Open(ctx, "large.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	buf := make([]byte, 4096)
	if n, err := r.ReadAt(buf, size-4096); n != 4096 || err != nil {
		t.Fatalf("tail read: %d %v", n, err)
	}
	if err := s.Delete(ctx, "large.bin"); err != nil {
		t.Error(err)
	}
}
