// Package epoch defines ISO-week epochs, the unit of deduplication and
// packaging, and the storage keys derived from them.
package epoch

import (
	"fmt"
	"strings"
	"time"
)

// ID identifies an epoch, e.g. "2026-W41".
type ID string

// Of returns the epoch containing t (ISO week, UTC).
func Of(t time.Time) ID {
	y, w := t.UTC().ISOWeek()
	return ID(fmt.Sprintf("%04d-W%02d", y, w))
}

// Parse validates s and returns it as an ID.
func Parse(s string) (ID, error) {
	var y, w int
	if _, err := fmt.Sscanf(s, "%4d-W%2d", &y, &w); err != nil || len(s) != 8 {
		return "", fmt.Errorf("epoch: invalid id %q", s)
	}
	id := ID(s)
	if Of(id.startOf(y, w)) != id {
		return "", fmt.Errorf("epoch: week out of range in %q", s)
	}
	return id, nil
}

// MustParse is Parse that panics on error.
func MustParse(s string) ID {
	id, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return id
}

func (id ID) parts() (int, int) {
	var y, w int
	_, _ = fmt.Sscanf(string(id), "%4d-W%2d", &y, &w)
	return y, w
}

func (id ID) startOf(y, w int) time.Time {
	jan4 := time.Date(y, 1, 4, 0, 0, 0, 0, time.UTC)
	offset := (int(jan4.Weekday()) + 6) % 7
	monday := jan4.AddDate(0, 0, -offset)
	return monday.AddDate(0, 0, (w-1)*7)
}

// Start is Monday 00:00 UTC of the epoch.
func (id ID) Start() time.Time {
	y, w := id.parts()
	return id.startOf(y, w)
}

// End is the start of the following epoch.
func (id ID) End() time.Time { return id.Start().AddDate(0, 0, 7) }

// Next returns the following epoch.
func (id ID) Next() ID { return Of(id.End()) }

// Prev returns the preceding epoch.
func (id ID) Prev() ID { return Of(id.Start().Add(-time.Hour)) }

// Year returns the ISO year.
func (id ID) Year() int { y, _ := id.parts(); return y }

// Week returns the ISO week number.
func (id ID) Week() int { _, w := id.parts(); return w }

func (id ID) path() string {
	y, w := id.parts()
	return fmt.Sprintf("%04d/W%02d", y, w)
}

// IncomingPrefix is the storage prefix for working capture files.
func (id ID) IncomingPrefix() string { return "incoming/" + id.path() + "/" }

// WeeklyKey is the storage key of the single WACZ package of weeks sealed
// before packages were split into volumes.
func (id ID) WeeklyKey() string { return "weekly/" + id.path() + ".wacz" }

// VolumeDir is the storage prefix of the week's volumes: WACZ packages of
// a bounded size that together hold the week.
func (id ID) VolumeDir() string { return "weekly/" + id.path() + "/" }

// VolumeKey is the storage key of volume n (from 1) of the week.
func (id ID) VolumeKey(n int) string { return fmt.Sprintf("%s%s-%04d.wacz", id.VolumeDir(), id, n) }

// VolumeCapturesKey is the storage key of the captures list of volume n.
func (id ID) VolumeCapturesKey(n int) string {
	return strings.TrimSuffix(id.VolumeKey(n), ".wacz") + ".captures.jsonl"
}

// SumsKey is the storage key of the SHA256SUMS file of the week's volumes.
func (id ID) SumsKey() string { return id.VolumeDir() + "SHA256SUMS" }

// CollectionKey is the storage key of the multi-WACZ collection listing
// the week's volumes (opened as one archive by ReplayWeb.page).
func (id ID) CollectionKey() string { return id.VolumeDir() + "collection.json" }

// ParquetKey is the storage key of the page table of a sealed epoch.
func (id ID) ParquetKey() string { return "weekly/" + id.path() + ".parquet" }

// CapturesKey is the storage key of the list of capture files packaged
// into the single package of a week sealed before volumes (see
// VolumeCapturesKey).
func (id ID) CapturesKey() string { return "weekly/" + id.path() + ".captures.jsonl" }

// ChecksumKey is the storage key of the package's sha256 file.
func (id ID) ChecksumKey() string { return id.WeeklyKey() + ".sha256" }

// CatalogKey is the storage key of the epoch status document.
func (id ID) CatalogKey() string { return "catalog/" + id.path() + ".json" }

// FromKey extracts the epoch from incoming/, weekly/ or catalog/ keys.
func FromKey(key string) (ID, bool) {
	var prefix string
	var y, w int
	for _, p := range []string{"incoming/", "weekly/", "catalog/"} {
		if len(key) > len(p) && key[:len(p)] == p {
			prefix = p
		}
	}
	if prefix == "" {
		return "", false
	}
	if _, err := fmt.Sscanf(key[len(prefix):], "%4d/W%2d", &y, &w); err != nil {
		return "", false
	}
	id, err := Parse(fmt.Sprintf("%04d-W%02d", y, w))
	return id, err == nil
}
