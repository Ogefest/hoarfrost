package archive

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ogefest/hoarfrost/internal/wacz"
)

// Capture files are fetched from storage ahead of packaging: a week holds
// up to millions of small files, and fetching them one after another
// spends most of the time waiting for each request.
const (
	prefetchWorkers  = 8
	prefetchBudget   = 256 << 20 // bytes held in memory at once
	prefetchMaxFile  = 32 << 20  // larger files are streamed when packaged
	prefetchAttempts = 3
)

// prefetcher downloads capture files in package order, holding at most
// budget bytes that the packaging has not read yet.
type prefetcher struct {
	mu     sync.Mutex
	cond   *sync.Cond
	used   int64
	budget int64
}

func (p *prefetcher) acquire(ctx context.Context, n int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.used > 0 && p.used+n > p.budget && ctx.Err() == nil {
		p.cond.Wait()
	}
	if ctx.Err() != nil {
		return false
	}
	p.used += n
	return true
}

func (p *prefetcher) release(n int64) {
	p.mu.Lock()
	p.used -= n
	p.mu.Unlock()
	p.cond.Broadcast()
}

type fetchedFile struct {
	done    chan struct{}
	data    []byte
	err     error
	release sync.Once
}

// prefetch returns inputs whose Open hands out the file downloaded ahead
// by workers. Opening must follow the order of inputs (as wacz.Build
// does with inputs in wacz.Order); files over maxFile are not fetched
// ahead. stop ends the workers and must be called once packaging ends.
func prefetch(ctx context.Context, inputs []wacz.Input, workers int, budget, maxFile int64) (out []wacz.Input, stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	p := &prefetcher{budget: budget}
	p.cond = sync.NewCond(&p.mu)
	out = make([]wacz.Input, len(inputs))
	files := make([]*fetchedFile, len(inputs))
	for i, in := range inputs {
		out[i] = in
		if in.Size > maxFile {
			continue
		}
		f := &fetchedFile{done: make(chan struct{})}
		files[i] = f
		size := in.Size
		free := func() { f.release.Do(func() { f.data = nil; p.release(size) }) }
		out[i].Open = func(octx context.Context) (io.ReadCloser, error) {
			select {
			case <-f.done:
			case <-octx.Done():
				return nil, octx.Err()
			}
			if f.err != nil {
				free()
				return nil, f.err
			}
			return &prefetchedReader{Reader: bytes.NewReader(f.data), free: free}, nil
		}
	}

	var wg sync.WaitGroup
	jobs := make(chan int)
	wg.Go(func() {
		defer close(jobs)
		for i, f := range files {
			if f == nil {
				continue
			}
			if !p.acquire(ctx, inputs[i].Size) {
				return
			}
			select {
			case jobs <- i:
			case <-ctx.Done():
				return
			}
		}
	})
	for range workers {
		wg.Go(func() {
			for i := range jobs {
				f := files[i]
				f.data, f.err = fetchCapture(ctx, inputs[i])
				close(f.done)
			}
		})
	}
	return out, func() {
		cancel()
		// Under the lock: a feeder that saw the context alive is waiting.
		p.mu.Lock()
		p.cond.Broadcast()
		p.mu.Unlock()
		wg.Wait()
	}
}

// fetchCapture reads a whole capture file, retrying failed requests.
func fetchCapture(ctx context.Context, in wacz.Input) ([]byte, error) {
	var err error
	for attempt := 1; attempt <= prefetchAttempts; attempt++ {
		var b []byte
		if b, err = readInput(ctx, in); err == nil {
			return b, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		select {
		case <-time.After(time.Duration(attempt) * time.Second):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("fetch %s: %w", in.Name, err)
}

func readInput(ctx context.Context, in wacz.Input) ([]byte, error) {
	rc, err := in.Open(ctx)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	buf := bytes.NewBuffer(make([]byte, 0, in.Size))
	if _, err := io.Copy(buf, rc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type prefetchedReader struct {
	*bytes.Reader
	free func()
}

func (r *prefetchedReader) Close() error {
	r.free()
	return nil
}
