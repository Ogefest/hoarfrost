package epoch

import (
	"testing"
	"time"
)

func TestOf(t *testing.T) {
	cases := []struct {
		t    time.Time
		want ID
	}{
		{time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC), "2026-W41"},
		{time.Date(2026, 10, 11, 23, 59, 59, 0, time.UTC), "2026-W41"},
		{time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC), "2026-W42"},
		{time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), "2026-W53"},
		{time.Date(2024, 12, 30, 0, 0, 0, 0, time.UTC), "2025-W01"},
		{time.Date(2026, 10, 12, 1, 0, 0, 0, time.FixedZone("CEST", 2*3600)), "2026-W41"},
	}
	for _, c := range cases {
		if got := Of(c.t); got != c.want {
			t.Errorf("Of(%v) = %s, want %s", c.t, got, c.want)
		}
	}
}

func TestBoundaries(t *testing.T) {
	id := MustParse("2026-W41")
	if !id.Start().Equal(time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("start %v", id.Start())
	}
	if !id.End().Equal(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("end %v", id.End())
	}
	if id.Next() != "2026-W42" || id.Prev() != "2026-W40" {
		t.Fatal("next/prev")
	}
	if MustParse("2026-W53").Next() != "2027-W01" || MustParse("2027-W01").Prev() != "2026-W53" {
		t.Fatal("year boundary")
	}
	if id.Year() != 2026 || id.Week() != 41 {
		t.Fatal("parts")
	}
	for d := MustParse("2020-W01"); d < "2030-W01"; d = d.Next() {
		if Of(d.Start()) != d || Of(d.End().Add(-time.Nanosecond)) != d {
			t.Fatalf("boundary mismatch for %s", d)
		}
	}
}

func TestParse(t *testing.T) {
	for _, bad := range []string{"", "2026-41", "2026-W54", "2025-W53", "2026-W00", "2026-W4", "2026-W41x"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) expected error", bad)
		}
	}
	if _, err := Parse("2026-W53"); err != nil {
		t.Error(err)
	}
}

func TestKeys(t *testing.T) {
	id := MustParse("2026-W01")
	if id.IncomingPrefix() != "incoming/2026/W01/" || id.WeeklyKey() != "weekly/2026/W01.wacz" ||
		id.ChecksumKey() != "weekly/2026/W01.wacz.sha256" || id.CatalogKey() != "catalog/2026/W01.json" {
		t.Fatal("keys")
	}
	for _, k := range []string{id.IncomingPrefix() + "x.warc.gz", id.WeeklyKey(), id.CatalogKey()} {
		if got, ok := FromKey(k); !ok || got != id {
			t.Errorf("FromKey(%q) = %s %v", k, got, ok)
		}
	}
	if _, ok := FromKey("config/sources/x.yaml"); ok {
		t.Error("FromKey accepted config key")
	}
}

func TestVolumeKeys(t *testing.T) {
	ep := MustParse("2026-W43")
	if got := ep.VolumeKey(7); got != "weekly/2026/W43/2026-W43-0007.wacz" {
		t.Fatal(got)
	}
	if got := ep.VolumeCapturesKey(7); got != "weekly/2026/W43/2026-W43-0007.captures.jsonl" {
		t.Fatal(got)
	}
	for _, k := range []string{ep.VolumeKey(1), ep.SumsKey(), ep.CollectionKey()} {
		if got, ok := FromKey(k); !ok || got != ep {
			t.Fatalf("%s: %s %v", k, got, ok)
		}
	}
}
