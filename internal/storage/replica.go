package storage

import (
	"context"
	"io"
	"log/slog"
	"sync"
)

// Replica writes to a primary backend and copies each new object to a
// secondary backend asynchronously. Reads are served from the primary.
type Replica struct {
	Primary   Storage
	Secondary Storage

	queue chan string
	wg    sync.WaitGroup
	log   *slog.Logger
	once  sync.Once
}

// NewReplica starts the background copier.
func NewReplica(primary, secondary Storage, log *slog.Logger) *Replica {
	if log == nil {
		log = slog.Default()
	}
	r := &Replica{Primary: primary, Secondary: secondary, queue: make(chan string, 1024), log: log}
	r.wg.Add(1)
	go r.run()
	return r
}

func (r *Replica) String() string {
	return r.Primary.String() + " (replica: " + r.Secondary.String() + ")"
}

func (r *Replica) run() {
	defer r.wg.Done()
	for key := range r.queue {
		if err := Copy(context.Background(), r.Primary, r.Secondary, key); err != nil {
			r.log.Error("replica copy failed", "key", key, "err", err)
		}
	}
}

// Copy copies one object between backends.
func Copy(ctx context.Context, from, to Storage, key string) error {
	src, err := from.Open(ctx, key)
	if err != nil {
		return err
	}
	defer src.Close()
	return to.Put(ctx, key, io.NewSectionReader(src, 0, src.Size()), src.Size())
}

func (r *Replica) Put(ctx context.Context, key string, rd io.Reader, size int64) error {
	if err := r.Primary.Put(ctx, key, rd, size); err != nil {
		return err
	}
	r.queue <- key
	return nil
}

func (r *Replica) Open(ctx context.Context, key string) (Reader, error) {
	return r.Primary.Open(ctx, key)
}

func (r *Replica) List(ctx context.Context, prefix string) ([]Object, error) {
	return r.Primary.List(ctx, prefix)
}

func (r *Replica) Stat(ctx context.Context, key string) (Object, error) {
	return r.Primary.Stat(ctx, key)
}

func (r *Replica) Delete(ctx context.Context, key string) error {
	if err := r.Primary.Delete(ctx, key); err != nil {
		return err
	}
	return r.Secondary.Delete(ctx, key)
}

// Close waits for pending copies to finish.
func (r *Replica) Close() error {
	r.once.Do(func() { close(r.queue) })
	r.wg.Wait()
	return nil
}
