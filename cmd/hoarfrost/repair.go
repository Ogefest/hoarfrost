package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/index"
)

// cmdRepairPublished recomputes publication dates from the structured data
// kept in the index and rewrites the page tables of the sealed weeks that
// changed.
func cmdRepairPublished(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("repair-published", flag.ContinueOnError)
	ep := epochFlag(fs)
	st := storageFlag(fs, "storage to repair (default: all)")
	dryRun := fs.Bool("dry-run", false, "only report the dates that would change")
	csvPath := fs.String("csv", "", "write the changes (page, epoch, source, url, old, new) to this CSV file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var id epoch.ID
	if *ep != "" {
		var err error
		if id, err = epoch.Parse(*ep); err != nil {
			return fmt.Errorf("-epoch: %w", err)
		}
	}
	in, err := openInstance(ctx)
	if err != nil {
		return err
	}
	defer in.close()
	arcs, err := in.archives(*st)
	if err != nil {
		return err
	}
	var all []index.PublishedFix
	for _, arc := range arcs {
		name := index.StorageOr(arc.Name)
		fixes, err := in.idx.RepairPublished(ctx, name, id, *dryRun, func(n int) {
			fmt.Fprintf(os.Stderr, "\r%s: %d page(s) scanned", name, n)
		})
		fmt.Fprintln(os.Stderr)
		all = append(all, fixes...)
		if err != nil {
			return err
		}
		changed := map[epoch.ID]int{}
		for _, f := range fixes {
			changed[f.Epoch]++
		}
		eps := make([]epoch.ID, 0, len(changed))
		for e := range changed {
			eps = append(eps, e)
		}
		sort.Slice(eps, func(i, j int) bool { return eps[i] < eps[j] })
		for _, e := range eps {
			info, err := in.idx.Epoch(ctx, name, e)
			if err != nil {
				return err
			}
			sealed := info.State == index.StateSealed || info.State == index.StateVerified
			action := "index only (open week)"
			if sealed && *dryRun {
				action = "page table would be rewritten"
			} else if sealed {
				pi, err := arc.RewriteParquet(ctx, e)
				if err != nil {
					return err
				}
				action = fmt.Sprintf("page table rewritten (%d rows, sha256 %s)", pi.Rows, pi.SHA256)
			}
			fmt.Printf("%s/%s: %d page(s) — %s\n", name, e, changed[e], action)
		}
	}
	bySource := map[string]int{}
	for _, f := range all {
		bySource[f.Source]++
	}
	sources := make([]string, 0, len(bySource))
	for s := range bySource {
		sources = append(sources, s)
	}
	sort.Slice(sources, func(i, j int) bool { return bySource[sources[i]] > bySource[sources[j]] })
	for i, s := range sources {
		if i == 20 {
			fmt.Printf("  … %d more source(s)\n", len(sources)-20)
			break
		}
		fmt.Printf("  %-40s %d\n", s, bySource[s])
	}
	verb := "changed"
	if *dryRun {
		verb = "would change"
	}
	fmt.Printf("%d publication date(s) %s\n", len(all), verb)
	if *csvPath != "" {
		if err := writeFixes(*csvPath, all); err != nil {
			return err
		}
	}
	return nil
}

func writeFixes(path string, fixes []index.PublishedFix) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	_ = w.Write([]string{"page_id", "epoch", "source", "url", "old", "new"})
	for _, x := range fixes {
		old := ""
		if !x.Old.IsZero() {
			old = x.Old.Format(time.RFC3339)
		}
		_ = w.Write([]string{strconv.FormatInt(x.PageID, 10), string(x.Epoch), x.Source, x.URI, old, x.New.Format(time.RFC3339)})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
