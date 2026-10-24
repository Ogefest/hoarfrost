package storage_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/ogefest/hoarfrost/internal/storage"
	"github.com/ogefest/hoarfrost/internal/storage/storagetest"
)

func TestFSContract(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Storage {
		s, err := storage.NewFS(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func newFakeS3(t *testing.T, prefix string) storage.Storage {
	backend := s3mem.New()
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	if err := backend.CreateBucket("archive"); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(srv.URL)
	s, err := storage.NewS3(context.Background(), storage.S3Config{
		Endpoint: u.Host, Bucket: "archive", Prefix: prefix, Region: "us-east-1",
		AccessKey: "key", SecretKey: "secret", PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestS3Contract(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Storage { return newFakeS3(t, "") })
}

func TestS3PrefixContract(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Storage { return newFakeS3(t, "press/archive") })
}

// TestS3RealContract runs against a real S3-compatible service, e.g. MinIO:
// HOARFROST_TEST_S3='s3://bucket/prefix?endpoint=http://localhost:9000&region=us-east-1&path_style=true'
func TestS3RealContract(t *testing.T) {
	raw := os.Getenv("HOARFROST_TEST_S3")
	if raw == "" {
		t.Skip("HOARFROST_TEST_S3 not set")
	}
	n := 0
	storagetest.Run(t, func(t *testing.T) storage.Storage {
		n++
		u, _ := url.Parse(raw)
		u.Path = strings.TrimSuffix(u.Path, "/") + "/contract-" + t.Name()[len("TestS3RealContract/"):]
		s, err := storage.Open(context.Background(), u.String())
		if err != nil {
			t.Fatal(err)
		}
		cleanup := func() {
			objs, _ := s.List(context.Background(), "")
			for _, o := range objs {
				s.Delete(context.Background(), o.Key)
			}
		}
		cleanup()
		t.Cleanup(cleanup)
		return s
	})
}

func TestReplicaContract(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Storage {
		p, _ := storage.NewFS(t.TempDir())
		s, _ := storage.NewFS(t.TempDir())
		r := storage.NewReplica(p, s, nil)
		t.Cleanup(func() { r.Close() })
		return r
	})
}

func TestReplicaCopies(t *testing.T) {
	ctx := context.Background()
	p, _ := storage.NewFS(t.TempDir())
	s, _ := storage.NewFS(t.TempDir())
	r := storage.NewReplica(p, s, nil)
	if err := storage.PutBytes(ctx, r, "weekly/2026/W41.wacz", []byte("data")); err != nil {
		t.Fatal(err)
	}
	r.Close()
	got, err := storage.ReadAll(ctx, s, "weekly/2026/W41.wacz")
	if err != nil || string(got) != "data" {
		t.Fatalf("replica copy: %q %v", got, err)
	}
}

func TestOpenURL(t *testing.T) {
	dir := t.TempDir()
	s, err := storage.Open(context.Background(), "file://"+dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(s.String(), dir) {
		t.Fatalf("String() = %s", s)
	}
	for _, bad := range []string{"ftp://x", "file://host/path", "s3://"} {
		if _, err := storage.Open(context.Background(), bad); err == nil {
			t.Errorf("Open(%q) expected error", bad)
		}
	}
}

type countingTransport struct {
	mu   sync.Mutex
	gets int
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodGet && r.Header.Get("Range") != "" {
		c.mu.Lock()
		c.gets++
		c.mu.Unlock()
	}
	return http.DefaultTransport.RoundTrip(r)
}

// TestS3SingleRangeRead checks that reading a record spanning cache blocks
// costs exactly one range request, and cached reads cost none.
func TestS3SingleRangeRead(t *testing.T) {
	backend := s3mem.New()
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	defer srv.Close()
	backend.CreateBucket("archive")
	u, _ := url.Parse(srv.URL)
	ct := &countingTransport{}
	s, err := storage.NewS3(context.Background(), storage.S3Config{Endpoint: u.Host, Bucket: "archive", Region: "us-east-1",
		AccessKey: "k", SecretKey: "s", PathStyle: true, Transport: ct})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	data := bytes.Repeat([]byte("0123456789"), 200_000)
	if err := storage.PutBytes(ctx, s, "weekly/2026/W41.wacz", data); err != nil {
		t.Fatal(err)
	}
	r, err := s.Open(ctx, "weekly/2026/W41.wacz")
	if err != nil {
		t.Fatal(err)
	}
	ct.gets = 0
	buf := make([]byte, 300_000)
	off := int64(256<<10 - 1000)
	if n, err := r.ReadAt(buf, off); n != len(buf) || err != nil {
		t.Fatal(n, err)
	}
	if !bytes.Equal(buf, data[off:off+int64(len(buf))]) {
		t.Fatal("content")
	}
	if ct.gets != 1 {
		t.Fatalf("%d range requests, want 1", ct.gets)
	}
	r.ReadAt(buf[:1000], off+10)
	if ct.gets != 1 {
		t.Fatal("cached read hit the network")
	}
}
