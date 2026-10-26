package wacz

import (
	"bufio"
	"compress/gzip"
	"container/heap"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
)

// DefaultSortMemory bounds the lines a package build keeps in memory before
// writing a sorted run to the spool directory.
const DefaultSortMemory = 64 << 20

// lineSorter sorts lines bytewise (like `LC_ALL=C sort`) in bounded memory:
// lines are collected up to a limit, then written as sorted, gzipped runs
// to temporary files, which are merged at the end. Lines must not contain
// '\n'.
type lineSorter struct {
	dir   string
	limit int
	lines []string
	size  int
	runs  []string
	n     int
}

func newLineSorter(dir string, limit int) *lineSorter {
	if dir == "" {
		dir = os.TempDir()
	}
	if limit <= 0 {
		limit = DefaultSortMemory
	}
	return &lineSorter{dir: dir, limit: limit}
}

func (s *lineSorter) add(line string) error {
	s.lines = append(s.lines, line)
	s.size += len(line) + 16
	s.n++
	if s.size >= s.limit {
		return s.spill()
	}
	return nil
}

// maxFanIn bounds the runs merged at once, and so the open files and
// buffers of a merge; more runs are merged in several passes.
const maxFanIn = 64

// spill writes the collected lines as a sorted run.
func (s *lineSorter) spill() error {
	sort.Strings(s.lines)
	lines := s.lines
	s.lines, s.size = nil, 0
	return s.writeRun(func(emit func(string) error) error {
		for _, l := range lines {
			if err := emit(l); err != nil {
				return err
			}
		}
		return nil
	})
}

// writeRun stores the lines produced by gen, already in order, as a run.
func (s *lineSorter) writeRun(gen func(emit func(string) error) error) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, "sort-*.gz")
	if err != nil {
		return err
	}
	s.runs = append(s.runs, f.Name())
	bw := bufio.NewWriterSize(f, 256<<10)
	gz, _ := gzip.NewWriterLevel(bw, gzip.BestSpeed)
	err = gen(func(l string) error {
		if _, err := io.WriteString(gz, l); err != nil {
			return err
		}
		_, err := gz.Write([]byte{'\n'})
		return err
	})
	if err == nil {
		err = gz.Close()
	}
	if err == nil {
		err = bw.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// each calls fn with every line in order.
func (s *lineSorter) each(fn func(line string) error) error {
	if len(s.runs) == 0 {
		sort.Strings(s.lines)
		for _, l := range s.lines {
			if err := fn(l); err != nil {
				return err
			}
		}
		return nil
	}
	if len(s.lines) > 0 {
		if err := s.spill(); err != nil {
			return err
		}
	}
	for len(s.runs) > maxFanIn {
		runs := s.runs
		s.runs = nil
		for i := 0; i < len(runs); i += maxFanIn {
			group := runs[i:min(i+maxFanIn, len(runs))]
			err := s.writeRun(func(emit func(string) error) error { return mergeRuns(group, emit) })
			for _, name := range group {
				os.Remove(name)
			}
			if err != nil {
				s.runs = append(s.runs, runs[i+len(group):]...)
				return err
			}
		}
	}
	return mergeRuns(s.runs, fn)
}

// mergeRuns calls fn with the lines of sorted run files in order.
func mergeRuns(names []string, fn func(line string) error) error {
	h := &mergeHeap{}
	defer h.close()
	for _, name := range names {
		f, err := os.Open(name)
		if err != nil {
			return err
		}
		gz, err := gzip.NewReader(bufio.NewReaderSize(f, 64<<10))
		if err != nil {
			f.Close()
			return err
		}
		c := &runCursor{r: bufio.NewReaderSize(gz, 64<<10), f: f}
		if err := c.next(); err != nil {
			f.Close()
			if errors.Is(err, io.EOF) {
				continue
			}
			return err
		}
		h.items = append(h.items, c)
	}
	heap.Init(h)
	for h.Len() > 0 {
		c := h.items[0]
		if err := fn(c.line); err != nil {
			return err
		}
		if err := c.next(); err != nil {
			if !errors.Is(err, io.EOF) {
				return err
			}
			heap.Pop(h)
			c.f.Close()
			continue
		}
		heap.Fix(h, 0)
	}
	return nil
}

// close removes the run files.
func (s *lineSorter) close() {
	for _, name := range s.runs {
		os.Remove(name)
	}
	s.runs, s.lines = nil, nil
}

// runCursor is the current line of a run file.
type runCursor struct {
	line string
	r    *bufio.Reader
	f    *os.File
}

func (c *runCursor) next() error {
	l, err := c.r.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && l != "" {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	c.line = strings.TrimSuffix(l, "\n")
	return nil
}

type mergeHeap struct{ items []*runCursor }

func (h *mergeHeap) Len() int           { return len(h.items) }
func (h *mergeHeap) Less(i, j int) bool { return h.items[i].line < h.items[j].line }
func (h *mergeHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *mergeHeap) Push(x any)         { h.items = append(h.items, x.(*runCursor)) }
func (h *mergeHeap) Pop() any {
	old := h.items
	x := old[len(old)-1]
	h.items = old[:len(old)-1]
	return x
}

func (h *mergeHeap) close() {
	for _, c := range h.items {
		c.f.Close()
	}
	h.items = nil
}
