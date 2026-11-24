package archive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/ogefest/hoarfrost/internal/wacz"
)

type fakeFiles struct {
	mu     sync.Mutex
	opened map[string]int
	fail   map[string]int // failures left per name
}

func (f *fakeFiles) inputs(sizes ...int) []wacz.Input {
	var out []wacz.Input
	for i, n := range sizes {
		name := fmt.Sprintf("f%03d", i)
		body := bytes.Repeat([]byte{byte('a' + i%26)}, n)
		out = append(out, wacz.Input{Name: name, Size: int64(n), Open: func(context.Context) (io.ReadCloser, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.opened[name]++
			if f.fail[name] > 0 {
				f.fail[name]--
				return nil, errors.New("connection reset")
			}
			return io.NopCloser(bytes.NewReader(body)), nil
		}})
	}
	return out
}

func newFakeFiles() *fakeFiles { return &fakeFiles{opened: map[string]int{}, fail: map[string]int{}} }

func TestPrefetchKeepsOrderAndBudget(t *testing.T) {
	f := newFakeFiles()
	ctx := context.Background()
	sizes := []int{10, 20, 30, 100, 5, 40, 40, 40, 1, 0, 25}
	in := f.inputs(sizes...)
	out, stop := prefetch(ctx, in, 4, 60, 50)
	defer stop()
	for i, o := range out {
		rc, err := o.Open(ctx)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		rc.Close()
		if len(b) != sizes[i] || (len(b) > 0 && b[0] != byte('a'+i%26)) {
			t.Fatalf("file %d: %d bytes", i, len(b))
		}
	}
	for name, n := range f.opened {
		if n != 1 {
			t.Fatalf("%s opened %d times", name, n)
		}
	}
}

func TestPrefetchRetriesAndReportsErrors(t *testing.T) {
	f := newFakeFiles()
	ctx := context.Background()
	in := f.inputs(10, 10, 10)
	f.fail["f001"] = 1
	f.fail["f002"] = prefetchAttempts
	out, stop := prefetch(ctx, in, 2, 100, 100)
	defer stop()
	for i, o := range out {
		rc, err := o.Open(ctx)
		if i == 2 {
			if err == nil {
				t.Fatal("f002 should fail")
			}
			continue
		}
		if err != nil {
			t.Fatalf("file %d: %v", i, err)
		}
		rc.Close()
	}
	if f.opened["f001"] != 2 {
		t.Fatalf("f001 opened %d times", f.opened["f001"])
	}
}

// TestPrefetchStopsUnread: packaging that fails early leaves files
// unread; stop must not wait for them.
func TestPrefetchStopsUnread(t *testing.T) {
	f := newFakeFiles()
	out, stop := prefetch(context.Background(), f.inputs(30, 30, 30, 30), 2, 40, 40)
	rc, err := out[0].Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	stop()
	stop()
}
