package wacz

import (
	"fmt"
	"math/rand"
	"os"
	"slices"
	"sort"
	"testing"
)

// TestLineSorter compares the sorter with sort.Strings at memory limits
// from one line per run to everything in memory, including merges in
// several passes, and checks that no run files are left.
func TestLineSorter(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	var lines []string
	for i := range 5000 {
		lines = append(lines, fmt.Sprintf("%c%x %d", 'a'+rng.Intn(3), rng.Int63n(1<<20), i%7))
	}
	lines = append(lines, lines[:50]...) // duplicates
	want := slices.Clone(lines)
	sort.Strings(want)
	for _, limit := range []int{1, 300, 5000, 1 << 30} {
		dir := t.TempDir()
		s := newLineSorter(dir, limit)
		for _, l := range lines {
			if err := s.add(l); err != nil {
				t.Fatal(err)
			}
		}
		var got []string
		if err := s.each(func(l string) error { got = append(got, l); return nil }); err != nil {
			t.Fatal(err)
		}
		s.close()
		if !slices.Equal(got, want) || s.n != len(lines) {
			t.Fatalf("limit %d: %d lines, order differs", limit, len(got))
		}
		if left, _ := os.ReadDir(dir); len(left) != 0 {
			t.Fatalf("limit %d: %d files left", limit, len(left))
		}
	}
}
