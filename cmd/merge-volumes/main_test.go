package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ogefest/hoarfrost/internal/archive"
	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/index"
)

// sealedWeek writes a fake sealed week of n volumes under root.
func sealedWeek(t *testing.T, root string, ep epoch.ID, payloads ...string) *archive.Catalog {
	t.Helper()
	c := &archive.Catalog{Epoch: ep, State: index.StateVerified, Sums: ep.SumsKey(), Collection: ep.CollectionKey(),
		Verification: &archive.Verification{}, Parquet: &archive.ParquetInfo{Key: ep.ParquetKey()}}
	os.MkdirAll(filepath.Join(root, ep.VolumeDir()), 0o755)
	os.MkdirAll(filepath.Dir(filepath.Join(root, ep.CatalogKey())), 0o755)
	for i, p := range payloads {
		h := sha256.Sum256([]byte(p))
		v := archive.Volume{Key: ep.VolumeKey(i + 1), CapturesList: ep.VolumeCapturesKey(i + 1), SHA256: hex.EncodeToString(h[:]), Size: int64(len(p)), Pages: 2, Captures: 1}
		os.WriteFile(filepath.Join(root, v.Key), []byte(p), 0o644)
		m := &archive.Manifest{Epoch: ep, Package: v.Key, SHA256: v.SHA256, Size: v.Size, Pages: 2, Indexed: 5, WARCFiles: 1,
			Captures: []archive.ManifestCapture{{Key: ep.IncomingPrefix() + p + ".warc.gz", File: p + ".warc.gz"}}}
		var b bytes.Buffer
		writeManifest(&b, m)
		os.WriteFile(filepath.Join(root, v.CapturesList), b.Bytes(), 0o644)
		c.Volumes = append(c.Volumes, v)
		c.Size += v.Size
		c.Pages += 2
	}
	sums := archive.VolumeSums(c.Volumes)
	coll, _ := archive.VolumeCollection(ep, c.Volumes)
	os.WriteFile(filepath.Join(root, c.Sums), sums, 0o644)
	os.WriteFile(filepath.Join(root, c.Collection), coll, 0o644)
	h := sha256.Sum256(sums)
	c.SHA256 = hex.EncodeToString(h[:])
	b, _ := json.MarshalIndent(c, "", "  ")
	os.WriteFile(filepath.Join(root, ep.CatalogKey()), b, 0o644)
	return c
}

func TestMergeWeek(t *testing.T) {
	ep := epoch.MustParse("2026-W10")
	dst, src, bak := t.TempDir(), t.TempDir(), t.TempDir()
	sealedWeek(t, dst, ep, "main-one", "main-two")
	add := sealedWeek(t, src, ep, "rescued")

	if err := mergeWeek(dst, src, bak, add, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, ep.VolumeKey(3))); err == nil {
		t.Fatal("dry run changed the week")
	}
	if err := mergeWeek(dst, src, bak, add, true); err != nil {
		t.Fatal(err)
	}
	var got archive.Catalog
	if err := readJSON(filepath.Join(dst, ep.CatalogKey()), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Volumes) != 3 || got.Volumes[2].Key != ep.VolumeKey(3) || got.Pages != 6 || got.Size != int64(len("main-onemain-tworescued")) {
		t.Fatalf("catalog %+v", got)
	}
	if got.State != index.StateSealed || got.Verification != nil || got.Parquet != nil {
		t.Fatalf("merged week still marked verified: %+v", got)
	}
	sums, _ := os.ReadFile(filepath.Join(dst, got.Sums))
	h := sha256.Sum256(sums)
	if !bytes.Equal(sums, archive.VolumeSums(got.Volumes)) || hex.EncodeToString(h[:]) != got.SHA256 {
		t.Fatal("SHA256SUMS does not match the catalog")
	}
	coll, _ := os.ReadFile(filepath.Join(dst, got.Collection))
	want, _ := archive.VolumeCollection(ep, got.Volumes)
	if !bytes.Equal(coll, want) {
		t.Fatal("collection does not match the catalog")
	}
	if b, _ := os.ReadFile(filepath.Join(dst, ep.VolumeKey(3))); string(b) != "rescued" {
		t.Fatal("volume not moved unchanged")
	}
	m, err := readManifest(filepath.Join(dst, ep.VolumeCapturesKey(3)))
	if err != nil || m.Package != ep.VolumeKey(3) || len(m.Captures) != 1 {
		t.Fatalf("captures list %+v %v", m, err)
	}
	if _, err := os.Stat(filepath.Join(src, ep.CatalogKey())); err == nil {
		t.Fatal("merged week left in the source")
	}
	if _, err := os.Stat(filepath.Join(bak, string(ep), "W10.json")); err != nil {
		t.Fatal("catalog not backed up")
	}
	// Running again finds nothing to do.
	if err := mergeWeek(dst, src, bak, add, true); err == nil {
		var again archive.Catalog
		readJSON(filepath.Join(dst, ep.CatalogKey()), &again)
		if len(again.Volumes) != 3 {
			t.Fatal("volume added twice")
		}
	}
}

func TestMergeRefusesUnverified(t *testing.T) {
	ep := epoch.MustParse("2026-W10")
	dst, src := t.TempDir(), t.TempDir()
	sealedWeek(t, dst, ep, "main")
	add := sealedWeek(t, src, ep, "rescued")
	add.State = index.StateSealed
	if err := mergeWeek(dst, src, t.TempDir(), add, true); err == nil {
		t.Fatal("unverified week merged")
	}
}
