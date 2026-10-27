package storage

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config configures an S3-compatible backend.
type S3Config struct {
	Endpoint  string // host[:port], without scheme
	Secure    bool
	Region    string
	Bucket    string
	Prefix    string
	AccessKey string
	SecretKey string
	PathStyle bool
	// VisibleWait bounds how long a completed streamed upload may take to
	// become visible (default 10 minutes).
	VisibleWait time.Duration
	// Transport overrides the HTTP transport (used by tests).
	Transport http.RoundTripper
}

// S3 stores objects in an S3-compatible bucket.
type S3 struct {
	cfg    S3Config
	client *minio.Client
}

// NewS3FromURL parses s3://bucket/prefix?endpoint=https://host&region=r[&path_style=true].
func NewS3FromURL(ctx context.Context, u *url.URL, accessKey, secretKey string) (*S3, error) {
	q := u.Query()
	cfg := S3Config{
		Bucket:    u.Host,
		Prefix:    strings.Trim(u.Path, "/"),
		Region:    q.Get("region"),
		AccessKey: accessKey,
		SecretKey: secretKey,
		Secure:    true,
		PathStyle: q.Get("path_style") == "true",
	}
	ep := q.Get("endpoint")
	if ep == "" {
		ep = "https://s3.amazonaws.com"
	}
	epu, err := url.Parse(ep)
	if err != nil || epu.Host == "" {
		return nil, fmt.Errorf("storage: bad s3 endpoint %q", ep)
	}
	cfg.Endpoint = epu.Host
	cfg.Secure = epu.Scheme != "http"
	return NewS3(ctx, cfg)
}

// NewS3 creates an S3 backend and checks that the bucket exists.
func NewS3(ctx context.Context, cfg S3Config) (*S3, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("storage: s3 bucket is required")
	}
	lookup := minio.BucketLookupAuto
	if cfg.PathStyle {
		lookup = minio.BucketLookupPath
	}
	base := cfg.Transport
	if base == nil {
		tr, err := minio.DefaultTransport(cfg.Secure)
		if err != nil {
			return nil, fmt.Errorf("storage: %w", err)
		}
		base = tr
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       cfg.Secure,
		Region:       cfg.Region,
		BucketLookup: lookup,
		Transport:    &s3LogTransport{base: base},
	})
	if err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	ok, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("storage: s3 bucket %s: %w", cfg.Bucket, err)
	}
	if !ok {
		return nil, fmt.Errorf("storage: s3 bucket %s does not exist", cfg.Bucket)
	}
	return &S3{cfg: cfg, client: client}, nil
}

func (s *S3) String() string {
	scheme := "https"
	if !s.cfg.Secure {
		scheme = "http"
	}
	return fmt.Sprintf("s3://%s/%s?endpoint=%s://%s", s.cfg.Bucket, s.cfg.Prefix, scheme, s.cfg.Endpoint)
}

func (s *S3) key(key string) string {
	if s.cfg.Prefix == "" {
		return key
	}
	return s.cfg.Prefix + "/" + key
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if err := ValidKey(key); err != nil {
		return err
	}
	cr := &countReader{r: r}
	opts := minio.PutObjectOptions{SendContentMd5: true, ContentType: "application/octet-stream"}
	var body io.Reader = cr
	// The reader must also be an io.ReaderAt: minio-go hashes other
	// readers by loading the whole object into memory.
	if f, ok := r.(interface {
		io.ReadSeeker
		io.ReaderAt
	}); ok && size >= 0 && size <= s3MaxSinglePut {
		// A file goes up in one request, stored as it arrives: no multipart
		// upload for the server to assemble (some servers limit the parts
		// of an upload or take minutes to complete a large one). Content-MD5
		// is computed by reading the file before sending it.
		opts.DisableMultipart, opts.DisableContentSha256 = true, true
		body, cr.n = f, size
	}
	info, err := s.client.PutObject(ctx, s.cfg.Bucket, s.key(key), body, size, opts)
	if err != nil {
		return fmt.Errorf("storage: put %s: %w", key, err)
	}
	if size >= 0 && (info.Size != size || cr.n != size) {
		_ = s.client.RemoveObject(context.WithoutCancel(ctx), s.cfg.Bucket, s.key(key), minio.RemoveObjectOptions{})
		return fmt.Errorf("storage: put %s: stored %d bytes, expected %d", key, info.Size, size)
	}
	st, err := s.statVisible(ctx, key)
	if err != nil {
		return fmt.Errorf("storage: verify %s: %w", key, err)
	}
	if st.Size != cr.n {
		return fmt.Errorf("storage: verify %s: size %d, sent %d", key, st.Size, cr.n)
	}
	return nil
}

// s3MaxSinglePut is the largest object S3 accepts in one PUT request.
const s3MaxSinglePut = 5 << 30

// Streamed uploads send this many parts at once; each holds a part-sized
// buffer in memory.
const s3UploadThreads = 3

// s3PartSize chooses the part size of a streamed upload: 16 MiB, or more
// when sizeHint would need over 9000 of the 10000 parts S3 allows. With an
// unknown size, 64 MiB parts allow objects up to 640 GB.
func s3PartSize(sizeHint int64) uint64 {
	const mib = 1 << 20
	if sizeHint <= 0 {
		return 64 * mib
	}
	p := (sizeHint/9000 + mib - 1) / mib * mib
	return uint64(min(max(p, 16*mib), 5<<30))
}

// Create uploads the object in parts while it is written; it becomes
// visible when the multipart upload completes in Commit. A previous
// unfinished upload of the same key (e.g. of a process that was killed) is
// removed first.
func (s *S3) Create(ctx context.Context, key string, sizeHint int64) (Writer, error) {
	if err := ValidKey(key); err != nil {
		return nil, err
	}
	_ = s.client.RemoveIncompleteUpload(ctx, s.cfg.Bucket, s.key(key))
	// Parts carry Content-MD5 instead of a streaming (aws-chunked) payload
	// signature, which not every S3-compatible server decodes.
	opts := minio.PutObjectOptions{
		ContentType: "application/octet-stream", SendContentMd5: true, DisableContentSha256: true,
		PartSize: s3PartSize(sizeHint), ConcurrentStreamParts: true, NumThreads: s3UploadThreads,
	}
	var stored minio.UploadInfo
	upload := func(ctx context.Context, r io.Reader) error {
		info, err := s.client.PutObject(ctx, s.cfg.Bucket, s.key(key), r, -1, opts)
		if err != nil {
			_ = s.client.RemoveIncompleteUpload(context.WithoutCancel(ctx), s.cfg.Bucket, s.key(key))
			return fmt.Errorf("storage: put %s: %w", key, err)
		}
		stored = info
		return nil
	}
	check := func(n int64) error {
		st, err := s.statVisible(ctx, key)
		if err != nil {
			return fmt.Errorf("storage: verify %s (upload completed: %d bytes, etag %s; %s): %w",
				key, stored.Size, stored.ETag, s.describe(ctx, key), err)
		}
		if st.Size != n {
			return fmt.Errorf("storage: verify %s: size %d, sent %d", key, st.Size, n)
		}
		return nil
	}
	return newPipeWriter(ctx, upload, check), nil
}

// describe tells what the bucket listing shows of key and of its pending
// multipart uploads, to explain an object missing after its upload.
func (s *S3) describe(ctx context.Context, key string) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	listed := "not listed"
	for o := range s.client.ListObjects(ctx, s.cfg.Bucket, minio.ListObjectsOptions{Prefix: s.key(key), Recursive: true}) {
		if o.Err != nil {
			listed = "listing failed: " + o.Err.Error()
			break
		}
		if o.Key == s.key(key) {
			listed = fmt.Sprintf("listed with %d bytes", o.Size)
		}
	}
	pending := 0
	for u := range s.client.ListIncompleteUploads(ctx, s.cfg.Bucket, s.key(key), false) {
		if u.Err != nil {
			return listed + "; listing uploads failed: " + u.Err.Error()
		}
		if u.Key == s.key(key) {
			pending++
		}
	}
	return fmt.Sprintf("%s, %d pending uploads", listed, pending)
}

// statVisible stats an object whose multipart upload has just completed.
// Some servers (e.g. several gateways or filers behind a load balancer)
// show a large object only after a while, so a missing object is looked
// up again, with growing pauses, for up to VisibleWait.
func (s *S3) statVisible(ctx context.Context, key string) (Object, error) {
	limit := s.cfg.VisibleWait
	if limit <= 0 {
		limit = 10 * time.Minute
	}
	deadline := time.Now().Add(limit)
	pause := min(time.Second, limit/20)
	for {
		st, err := s.Stat(ctx, key)
		if !errors.Is(err, ErrNotExist) || time.Now().Add(pause).After(deadline) {
			if err != nil && errors.Is(err, ErrNotExist) {
				err = fmt.Errorf("%w (still missing after %s)", err, limit)
			}
			return st, err
		}
		slog.Warn("s3 object not visible after its upload; waiting", "key", key, "pause", pause)
		select {
		case <-ctx.Done():
			return Object{}, ctx.Err()
		case <-time.After(pause):
		}
		pause = min(pause*2, 30*time.Second)
	}
}

// Get reads the object with one streaming request, resuming from the
// current position after a broken connection.
func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	if err := ValidKey(key); err != nil {
		return nil, 0, err
	}
	info, err := s.client.StatObject(ctx, s.cfg.Bucket, s.key(key), minio.StatObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil, 0, fmt.Errorf("%w: %s", ErrNotExist, key)
		}
		return nil, 0, err
	}
	return &s3Stream{ctx: ctx, s: s, key: s.key(key), size: info.Size, etag: info.ETag}, info.Size, nil
}

// s3StreamRetries bounds reconnections in a row without data in between.
const s3StreamRetries = 5

type s3Stream struct {
	ctx     context.Context
	s       *S3
	key     string
	size    int64
	etag    string // a resumed read must get the same object
	pos     int64
	obj     *minio.Object
	retries int
}

func (r *s3Stream) Read(p []byte) (int, error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}
	for {
		if r.obj == nil {
			opts := minio.GetObjectOptions{}
			if r.etag != "" {
				if err := opts.SetMatchETag(r.etag); err != nil {
					return 0, err
				}
			}
			if r.pos > 0 {
				if err := opts.SetRange(r.pos, r.size-1); err != nil {
					return 0, err
				}
			}
			obj, err := r.s.client.GetObject(r.ctx, r.s.cfg.Bucket, r.key, opts)
			if err != nil {
				return 0, err
			}
			r.obj = obj
		}
		n, err := r.obj.Read(p)
		r.pos += int64(n)
		if n > 0 {
			r.retries = 0
		}
		if err == nil || (errors.Is(err, io.EOF) && r.pos == r.size) {
			return n, err
		}
		if errors.Is(err, io.EOF) {
			err = fmt.Errorf("storage: %s ended at %d of %d bytes", r.key, r.pos, r.size)
		}
		r.obj.Close()
		r.obj = nil
		if r.ctx.Err() != nil || r.retries >= s3StreamRetries {
			return n, err
		}
		r.retries++
		select {
		case <-r.ctx.Done():
			return n, r.ctx.Err()
		case <-time.After(time.Duration(r.retries) * time.Second):
		}
		if n > 0 {
			return n, nil
		}
	}
}

func (r *s3Stream) Close() error {
	if r.obj != nil {
		return r.obj.Close()
	}
	return nil
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (s *S3) Stat(ctx context.Context, key string) (Object, error) {
	if err := ValidKey(key); err != nil {
		return Object{}, err
	}
	info, err := s.client.StatObject(ctx, s.cfg.Bucket, s.key(key), minio.StatObjectOptions{})
	if err != nil {
		if isNotFound(err) {
			return Object{}, fmt.Errorf("%w: %s", ErrNotExist, key)
		}
		return Object{}, err
	}
	return Object{Key: key, Size: info.Size, SHA256: info.ChecksumSHA256}, nil
}

func (s *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	full := s.key(prefix)
	if s.cfg.Prefix != "" && prefix == "" {
		full = s.cfg.Prefix + "/"
	}
	for obj := range s.client.ListObjects(ctx, s.cfg.Bucket, minio.ListObjectsOptions{Prefix: full, Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		key := obj.Key
		if s.cfg.Prefix != "" {
			key = strings.TrimPrefix(key, s.cfg.Prefix+"/")
		}
		out = append(out, Object{Key: key, Size: obj.Size})
	}
	return out, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	if err := ValidKey(key); err != nil {
		return err
	}
	err := s.client.RemoveObject(ctx, s.cfg.Bucket, s.key(key), minio.RemoveObjectOptions{})
	if err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

func isNotFound(err error) bool {
	code := minio.ToErrorResponse(err).Code
	return code == "NoSuchKey" || code == "NotFound"
}

// Block cache parameters for S3 range reads.
const (
	s3BlockSize   = 256 << 10
	s3CacheBlocks = 64
	s3DirectLimit = 4 * s3BlockSize
)

func (s *S3) Open(ctx context.Context, key string) (Reader, error) {
	st, err := s.Stat(ctx, key)
	if err != nil {
		return nil, err
	}
	return &s3Reader{ctx: context.WithoutCancel(ctx), s: s, key: s.key(key), size: st.Size,
		cache: map[int64]*list.Element{}, lru: list.New()}, nil
}

type s3Reader struct {
	ctx  context.Context
	s    *S3
	key  string
	size int64

	mu    sync.Mutex
	cache map[int64]*list.Element
	lru   *list.List
}

type cachedBlock struct {
	idx  int64
	data []byte
}

func (r *s3Reader) Size() int64  { return r.size }
func (r *s3Reader) Close() error { return nil }

func (r *s3Reader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("storage: negative offset")
	}
	if off >= r.size {
		return 0, io.EOF
	}
	want := p
	if rem := r.size - off; int64(len(want)) > rem {
		want = want[:rem]
	}
	var n int
	var err error
	if len(want) > s3DirectLimit {
		n, err = r.rangeRead(want, off)
	} else {
		n, err = r.cachedRead(want, off)
	}
	if err == nil && n < len(p) {
		err = io.EOF
	}
	return n, err
}

func (r *s3Reader) rangeRead(p []byte, off int64) (int, error) {
	opts := minio.GetObjectOptions{}
	if err := opts.SetRange(off, off+int64(len(p))-1); err != nil {
		return 0, err
	}
	obj, err := r.s.client.GetObject(r.ctx, r.s.cfg.Bucket, r.key, opts)
	if err != nil {
		return 0, err
	}
	defer obj.Close()
	n, err := io.ReadFull(obj, p)
	if errors.Is(err, io.ErrUnexpectedEOF) {
		err = io.EOF
	}
	return n, err
}

// cachedRead serves p from cached blocks, fetching all missing blocks of the
// range with a single request.
func (r *s3Reader) cachedRead(p []byte, off int64) (int, error) {
	first, last := off/s3BlockSize, (off+int64(len(p))-1)/s3BlockSize
	if err := r.fill(first, last); err != nil {
		return 0, err
	}
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		idx := pos / s3BlockSize
		block, err := r.block(idx)
		if err != nil {
			return n, err
		}
		start := pos - idx*s3BlockSize
		if start >= int64(len(block)) {
			return n, io.EOF
		}
		n += copy(p[n:], block[start:])
	}
	return n, nil
}

// fill loads the missing blocks between first and last in one range read.
func (r *s3Reader) fill(first, last int64) error {
	r.mu.Lock()
	lo, hi := int64(-1), int64(-1)
	for i := first; i <= last; i++ {
		if _, ok := r.cache[i]; !ok {
			if lo < 0 {
				lo = i
			}
			hi = i
		}
	}
	r.mu.Unlock()
	if lo < 0 {
		return nil
	}
	start := lo * s3BlockSize
	end := min((hi+1)*s3BlockSize, r.size)
	buf := make([]byte, end-start)
	if _, err := r.rangeRead(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := lo; i <= hi; i++ {
		b := buf[(i-lo)*s3BlockSize : min((i-lo+1)*s3BlockSize, int64(len(buf)))]
		r.put(i, b)
	}
	return nil
}

func (r *s3Reader) put(idx int64, data []byte) {
	if el, ok := r.cache[idx]; ok {
		r.lru.MoveToFront(el)
		return
	}
	r.cache[idx] = r.lru.PushFront(&cachedBlock{idx, data})
	for r.lru.Len() > s3CacheBlocks {
		old := r.lru.Remove(r.lru.Back()).(*cachedBlock)
		delete(r.cache, old.idx)
	}
}

func (r *s3Reader) block(idx int64) ([]byte, error) {
	r.mu.Lock()
	if el, ok := r.cache[idx]; ok {
		r.lru.MoveToFront(el)
		r.mu.Unlock()
		return el.Value.(*cachedBlock).data, nil
	}
	r.mu.Unlock()

	start := idx * s3BlockSize
	size := min(int64(s3BlockSize), r.size-start)
	buf := make([]byte, size)
	if _, err := r.rangeRead(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.put(idx, buf)
	return buf, nil
}
