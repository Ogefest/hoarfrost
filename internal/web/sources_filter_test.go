package web

import (
	"testing"

	"github.com/ogefest/hoarfrost/internal/config"
)

func TestSourceRowMatches(t *testing.T) {
	r := sourceRow{Source: config.Source{Name: "pb_pl_archive", Sitemaps: []string{"https://www.pb.pl/sitemap/index"}}}
	for q, want := range map[string]bool{
		"":                true,
		"pb":              true,
		"PB_PL":           true,
		"pl_arch":         true,
		"pb archive":      true,
		"archive pb":      true,
		"pb.pl":           true,
		"  pb   archive ": true,
		"pb sitemap":      true, // words may match different fields
		"www.pb.pl":       true,
		"pb wnp":          false,
		"pbpl":            false,
		"rss":             false,
	} {
		if got := r.matches(q); got != want {
			t.Errorf("matches(%q) = %v, want %v", q, got, want)
		}
	}
}
