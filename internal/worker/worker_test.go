package worker

import (
	"strings"
	"testing"

	"github.com/ogefest/hoarfrost/internal/blocklist"
	"github.com/ogefest/hoarfrost/internal/engine"
)

func TestBlockRule(t *testing.T) {
	e, _ := blocklist.Parse(strings.NewReader("0.0.0.0 tabloid.example\n||tracker.example^\n"))
	set := blocklist.Compile([]blocklist.Entries{e}, nil)
	job := engine.Job{URL: "https://Tabloid.example/artykul", Options: engine.Options{Block: []string{"ads.local"}}}
	rule := blockRule(job, set)
	cases := map[string]bool{
		"tabloid.example":      false, // the archived page's own host
		"cdn.tracker.example":  true,
		"img.ads.local":        true, // source block list applies to every host
		"static.other.example": false,
	}
	for host, want := range cases {
		if got := rule(host); got != want {
			t.Errorf("rule(%q) = %v, want %v", host, got, want)
		}
	}
	own := blockRule(engine.Job{URL: "https://ads.local/x", Options: engine.Options{Block: []string{"ads.local"}}}, blocklist.Empty())
	if !own("ads.local") {
		t.Fatal("explicit source blocks apply even to the page host")
	}
}
