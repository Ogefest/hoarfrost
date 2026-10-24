package warcio

import (
	"bytes"
	"compress/gzip"
	"io"
)

// Writer writes records as consecutive gzip members.
type Writer struct {
	w      io.Writer
	offset int64
	gz     *gzip.Writer
	buf    bytes.Buffer
}

// NewWriter returns a Writer starting at offset 0 of w.
func NewWriter(w io.Writer) *Writer {
	gz, _ := gzip.NewWriterLevel(nil, gzip.DefaultCompression)
	return &Writer{w: w, gz: gz}
}

// Offset is the number of bytes written so far.
func (w *Writer) Offset() int64 { return w.offset }

// WriteRecord finalizes r if needed and writes it as one gzip member.
// It returns the member offset and compressed length.
func (w *Writer) WriteRecord(r *Record) (offset, length int64, err error) {
	if r.Header.Get(HContentLength) == "" || r.Header.Get(HBlockDigest) == "" {
		r.Finalize()
	}
	w.buf.Reset()
	w.gz.Reset(&w.buf)
	if err := encodeRecord(w.gz, r); err != nil {
		return 0, 0, err
	}
	if err := w.gz.Close(); err != nil {
		return 0, 0, err
	}
	offset = w.offset
	n, err := w.w.Write(w.buf.Bytes())
	w.offset += int64(n)
	if err != nil {
		return 0, 0, err
	}
	return offset, int64(n), nil
}

// Encode writes r uncompressed to w.
func Encode(w io.Writer, r *Record) error {
	if r.Header.Get(HContentLength) == "" {
		r.Finalize()
	}
	return encodeRecord(w, r)
}

func encodeRecord(w io.Writer, r *Record) error {
	if err := r.Header.writeTo(w); err != nil {
		return err
	}
	if _, err := w.Write(r.Block); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\r\n\r\n")
	return err
}
