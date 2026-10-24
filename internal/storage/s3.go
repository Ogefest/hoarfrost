package storage

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

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
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       cfg.Secure,
		Region:       cfg.Region,
		BucketLookup: lookup,
		Transport:    cfg.Transport,
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
	info, err := s.client.PutObject(ctx, s.cfg.Bucket, s.key(key), cr, size, minio.PutObjectOptions{
		SendContentMd5: true,
		ContentType:    "application/octet-stream",
	})
	if err != nil {
		return fmt.Errorf("storage: put %s: %w", key, err)
	}
	if size >= 0 && (info.Size != size || cr.n != size) {
		_ = s.client.RemoveObject(context.WithoutCancel(ctx), s.cfg.Bucket, s.key(key), minio.RemoveObjectOptions{})
		return fmt.Errorf("storage: put %s: stored %d bytes, expected %d", key, info.Size, size)
	}
	st, err := s.Stat(ctx, key)
	if err != nil {
		return fmt.Errorf("storage: verify %s: %w", key, err)
	}
	if st.Size != cr.n {
		return fmt.Errorf("storage: verify %s: size %d, sent %d", key, st.Size, cr.n)
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
