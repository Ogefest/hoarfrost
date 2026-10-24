package warcio

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
)

// DefaultMaxBlock bounds the size of a record block held in memory.
const DefaultMaxBlock = 1 << 30

// ErrBlockTooLarge is returned for records exceeding the reader limit.
var ErrBlockTooLarge = errors.New("warcio: record block too large")

type countingReader struct {
	r *bufio.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

// Reader iterates over records of a WARC file, gzipped per record or plain.
type Reader struct {
	src      *countingReader
	gz       *gzip.Reader
	gzipped  bool
	started  bool
	plain    *bufio.Reader
	MaxBlock int64
}

// NewReader returns a Reader. Compression is detected from the first bytes.
func NewReader(r io.Reader) *Reader {
	return &Reader{src: &countingReader{r: bufio.NewReaderSize(r, 64<<10)}, MaxBlock: DefaultMaxBlock}
}

// Next returns the next record with its offset and on-disk length.
// It returns io.EOF after the last record.
func (rd *Reader) Next() (rec *Record, offset, length int64, err error) {
	if !rd.started {
		rd.started = true
		magic, err := rd.src.r.Peek(2)
		if err != nil && len(magic) == 0 {
			return nil, 0, 0, io.EOF
		}
		rd.gzipped = len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b
		if !rd.gzipped {
			rd.plain = bufio.NewReader(rd.src)
		}
	}
	if rd.gzipped {
		return rd.nextGzip()
	}
	return rd.nextPlain()
}

func (rd *Reader) nextGzip() (*Record, int64, int64, error) {
	if _, err := rd.src.r.Peek(1); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, 0, 0, io.EOF
		}
		return nil, 0, 0, err
	}
	offset := rd.src.n
	var err error
	if rd.gz == nil {
		rd.gz, err = gzip.NewReader(rd.src)
	} else {
		err = rd.gz.Reset(rd.src)
	}
	if err != nil {
		return nil, offset, 0, fmt.Errorf("warcio: gzip member at %d: %w", offset, err)
	}
	rd.gz.Multistream(false)
	br := bufio.NewReader(rd.gz)
	rec, err := parseRecord(br, rd.MaxBlock)
	if err != nil {
		return nil, offset, 0, fmt.Errorf("warcio: record at %d: %w", offset, err)
	}
	if _, err := io.Copy(io.Discard, br); err != nil {
		return nil, offset, 0, fmt.Errorf("warcio: record at %d: %w", offset, err)
	}
	return rec, offset, rd.src.n - offset, nil
}

func (rd *Reader) nextPlain() (*Record, int64, int64, error) {
	for {
		b, err := rd.plain.Peek(1)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, 0, 0, io.EOF
			}
			return nil, 0, 0, err
		}
		if b[0] != '\r' && b[0] != '\n' {
			break
		}
		_, _ = rd.plain.ReadByte()
	}
	offset := rd.src.n - int64(rd.plain.Buffered())
	rec, err := parseRecord(rd.plain, rd.MaxBlock)
	if err != nil {
		return nil, offset, 0, fmt.Errorf("warcio: record at %d: %w", offset, err)
	}
	end := rd.src.n - int64(rd.plain.Buffered())
	return rec, offset, end - offset, nil
}

func parseRecord(r *bufio.Reader, maxBlock int64) (*Record, error) {
	h, err := readHeader(r)
	if err != nil {
		return nil, err
	}
	n, err := h.contentLength()
	if err != nil {
		return nil, err
	}
	if maxBlock > 0 && n > maxBlock {
		return nil, ErrBlockTooLarge
	}
	var buf bytes.Buffer
	if _, err := io.CopyN(&buf, r, n); err != nil {
		return nil, noEOF(err)
	}
	skipNewlines(r, 4)
	return &Record{Header: h, Block: buf.Bytes()}, nil
}

// skipNewlines consumes up to max CR or LF bytes.
func skipNewlines(r *bufio.Reader, max int) {
	for range max {
		if b, err := r.Peek(1); err != nil || (b[0] != '\r' && b[0] != '\n') {
			return
		}
		_, _ = r.ReadByte()
	}
}

// ReadRecordAt reads one record stored at [offset, offset+length) of ra.
func ReadRecordAt(ra io.ReaderAt, offset, length int64) (*Record, error) {
	rd := NewReader(io.NewSectionReader(ra, offset, length))
	rec, _, _, err := rd.Next()
	if errors.Is(err, io.EOF) {
		return nil, io.ErrUnexpectedEOF
	}
	return rec, err
}

// ReadAll returns every record of a WARC stream.
func ReadAll(r io.Reader) ([]*Record, error) {
	rd := NewReader(r)
	var out []*Record
	for {
		rec, _, _, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, rec)
	}
}
