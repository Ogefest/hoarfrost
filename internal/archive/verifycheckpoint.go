package archive

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ogefest/hoarfrost/internal/wacz"
)

// verifyCheckpoint is what a verification found in the volumes it has
// read, kept in the spool directory after each volume so that an
// interrupted verification of a large week goes on with the next volume
// instead of reading the week again. It belongs to one catalog.
type verifyCheckpoint struct {
	Catalog  string    `json:"catalog"` // SHA-256 of the catalog's packages
	SealedAt time.Time `json:"sealedAt"`
	Packages int       `json:"packages"` // number of packages of the catalog
	Verified int       `json:"verified"` // packages read, in catalog order

	At           time.Time      `json:"at"`
	Scanned      int            `json:"scanned"`
	Problems     []wacz.Problem `json:"problems,omitempty"`
	MoreProblems int            `json:"moreProblems,omitempty"`
	Done         int64          `json:"done"`
	Matched      int64          `json:"matched"`
	CDXJLines    int64          `json:"cdxjLines"`
	PageLines    int64          `json:"pageLines"`
	Segments     int            `json:"segments"`
	HasLists     bool           `json:"hasLists"`
}

// verifyCheckpointPath is the checkpoint file of a week, "" without a
// spool directory.
func (a *Archive) verifyCheckpointPath(cat *Catalog) string {
	if a.SpoolDir == "" {
		return ""
	}
	name := strings.NewReplacer("/", "_", "\\", "_").Replace(a.name())
	return filepath.Join(a.SpoolDir, "verify-"+name+"-"+string(cat.Epoch)+".json")
}

// loadVerifyCheckpoint returns the checkpoint of the catalog's
// verification, or nil when there is none for this catalog.
func (a *Archive) loadVerifyCheckpoint(cat *Catalog) *verifyCheckpoint {
	p := a.verifyCheckpointPath(cat)
	if p == "" {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			a.Log.Warn("reading the verification checkpoint", "epoch", cat.Epoch, "err", err)
		}
		return nil
	}
	var c verifyCheckpoint
	if err := json.Unmarshal(b, &c); err != nil {
		a.Log.Warn("ignoring an unreadable verification checkpoint", "epoch", cat.Epoch, "err", err)
		return nil
	}
	n := len(cat.Packages())
	if c.Catalog != cat.SHA256 || !c.SealedAt.Equal(cat.SealedAt) || c.Packages != n || c.Verified <= 0 || c.Verified >= n {
		return nil
	}
	return &c
}

// saveVerifyCheckpoint writes the checkpoint after verified packages.
func (vr *verifier) saveCheckpoint(verified int) {
	p := vr.a.verifyCheckpointPath(vr.cat)
	if p == "" {
		return
	}
	c := verifyCheckpoint{
		Catalog: vr.cat.SHA256, SealedAt: vr.cat.SealedAt, Packages: len(vr.cat.Packages()), Verified: verified,
		At: vr.v.At, Scanned: vr.v.Scanned, Problems: vr.v.Problems, MoreProblems: vr.v.MoreProblems,
		Done: vr.done, Matched: vr.matched, CDXJLines: vr.cdxjLines, PageLines: vr.pageLines, Segments: vr.segments, HasLists: vr.hasLists,
	}
	b, err := json.Marshal(c)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(p), 0o755)
	}
	if err == nil {
		tmp := p + ".tmp"
		if err = os.WriteFile(tmp, b, 0o644); err == nil {
			err = os.Rename(tmp, p)
		}
	}
	if err != nil {
		// Only resuming is lost: the verification goes on.
		vr.a.Log.Warn("writing the verification checkpoint", "epoch", vr.cat.Epoch, "err", err)
	}
}

func (vr *verifier) restore(c *verifyCheckpoint) {
	vr.v.At, vr.v.Scanned, vr.v.Problems, vr.v.MoreProblems = c.At, c.Scanned, c.Problems, c.MoreProblems
	vr.done, vr.matched, vr.cdxjLines, vr.pageLines, vr.segments, vr.hasLists = c.Done, c.Matched, c.CDXJLines, c.PageLines, c.Segments, c.HasLists
}

func (a *Archive) removeVerifyCheckpoint(cat *Catalog) {
	if p := a.verifyCheckpointPath(cat); p != "" {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			a.Log.Warn("removing the verification checkpoint", "epoch", cat.Epoch, "err", err)
		}
	}
}
