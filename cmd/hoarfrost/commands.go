package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/ogefest/hoarfrost/internal/archive"
	"github.com/ogefest/hoarfrost/internal/auth"
	"github.com/ogefest/hoarfrost/internal/config"
	"github.com/ogefest/hoarfrost/internal/epoch"
	"github.com/ogefest/hoarfrost/internal/export"
	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/legacy"
	"github.com/ogefest/hoarfrost/internal/storage"
	"github.com/ogefest/hoarfrost/internal/worker"
)

func cmdWorker(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	coord := fs.String("coordinator", "", "coordinator URL (default HOARFROST_COORDINATOR)")
	slots := fs.Int("slots", 0, "parallel captures (default HOARFROST_WORKERS)")
	engineNames := fs.String("engines", "", "comma-separated engines to offer (default: all available)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	env, err := config.FromEnvironment()
	if err != nil {
		return err
	}
	log := newLogger(env)
	if *coord == "" {
		*coord = env.Coordinator
	}
	if *coord == "" || env.WorkerToken == "" {
		return errors.New("worker needs HOARFROST_COORDINATOR (or -coordinator) and HOARFROST_WORKER_TOKEN")
	}
	if *slots == 0 {
		*slots = env.Workers
	}
	stores, closeStores, err := openStorages(ctx, env, log)
	if err != nil {
		return err
	}
	defer closeStores()
	reg, closeEngines := engines(env, *engineNames, log)
	defer closeEngines()
	w := &worker.Worker{Name: env.WorkerName, Coord: worker.NewHTTPCoordinator(*coord, env.WorkerToken), Storages: stores,
		Engines: reg, SpoolDir: filepath.Join(env.DataDir, "spool"), Slots: *slots, Log: log}
	log.Info("worker started", "name", w.Name, "coordinator", *coord, "engines", reg.Names(), "storages", env.StorageNames(), "slots", *slots)
	return w.Run(ctx)
}

func readPassword(prompt string, stdin bool) (string, error) {
	if stdin || !term.IsTerminal(int(os.Stdin.Fd())) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, "Repeat: ")
	again, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if string(b) != string(again) {
		return "", errors.New("passwords do not match")
	}
	return string(b), nil
}

func cmdUser(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("user", flag.ContinueOnError)
	role := fs.String("role", auth.RoleViewer, "role for add/role: viewer, editor or admin")
	pwStdin := fs.Bool("password-stdin", false, "read the password from standard input")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: hoarfrost user [flags] add|reset-password|delete|role|token <login> [token name] | list")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return flag.ErrHelp
	}
	in, err := openInstance(ctx)
	if err != nil {
		return err
	}
	defer in.close()
	if err := in.env.RequireSecret(); err != nil {
		return err
	}
	login := ""
	if len(rest) > 1 {
		login = rest[1]
	}
	need := func() error {
		if login == "" {
			return errors.New("login required")
		}
		return nil
	}
	switch rest[0] {
	case "list":
		users, err := in.auth.Users(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "LOGIN\tROLE\tCREATED")
		for _, u := range users {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", u.Login, u.Role, u.Created.Format(time.DateOnly))
		}
		return tw.Flush()
	case "add":
		if err := need(); err != nil {
			return err
		}
		pw, err := readPassword("Password: ", *pwStdin)
		if err != nil {
			return err
		}
		if _, err := in.auth.CreateUser(ctx, login, pw, *role); err != nil {
			return err
		}
		fmt.Printf("user %s created with role %s\n", login, *role)
	case "reset-password":
		if err := need(); err != nil {
			return err
		}
		pw, err := readPassword("New password: ", *pwStdin)
		if err != nil {
			return err
		}
		if err := in.auth.SetPassword(ctx, login, pw); err != nil {
			return err
		}
		fmt.Printf("password of %s changed, sessions ended\n", login)
	case "role":
		if err := need(); err != nil {
			return err
		}
		if err := in.auth.SetRole(ctx, login, *role); err != nil {
			return err
		}
		fmt.Printf("%s is now %s\n", login, *role)
	case "delete":
		if err := need(); err != nil {
			return err
		}
		return in.auth.DeleteUser(ctx, login)
	case "token":
		if err := need(); err != nil {
			return err
		}
		users, err := in.auth.Users(ctx)
		if err != nil {
			return err
		}
		name := "cli"
		if len(rest) > 2 {
			name = rest[2]
		}
		for _, u := range users {
			if u.Login == login {
				tok, err := in.auth.CreateToken(ctx, u, name)
				if err != nil {
					return err
				}
				fmt.Println(tok)
				return nil
			}
		}
		return auth.ErrNotFound
	default:
		fs.Usage()
		return flag.ErrHelp
	}
	return nil
}

func storageFlag(fs *flag.FlagSet, what string) *string {
	return fs.String("storage", "", what)
}

func epochFlag(fs *flag.FlagSet) *string {
	return fs.String("epoch", "", "epoch (ISO week), e.g. "+string(epoch.Of(time.Now())))
}

func cmdSeal(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("seal", flag.ContinueOnError)
	ep := epochFlag(fs)
	st := storageFlag(fs, "storage to seal (default: the default storage)")
	force := fs.Bool("force", false, "seal even if captures of the epoch are still leased")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := epoch.Parse(*ep)
	if err != nil {
		return fmt.Errorf("-epoch: %w", err)
	}
	in, err := openInstance(ctx)
	if err != nil {
		return err
	}
	defer in.close()
	cat, err := in.coord.Seal(ctx, *st, id, *force)
	if err != nil {
		return err
	}
	fmt.Printf("%s/%s sealed: %s (%d volume(s), %d bytes, %d captures, %d records, %d pages), state %s\n", index.StorageOr(*st), id, cat.Location(),
		len(cat.Packages()), cat.Size, cat.Captures, cat.Records, cat.Pages, cat.State)
	if cat.State != index.StateVerified {
		return exitCode(1)
	}
	return nil
}

func cmdVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	ep := epochFlag(fs)
	st := storageFlag(fs, "storage to verify (default: all)")
	if err := fs.Parse(args); err != nil {
		return err
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
	failed := 0
	for _, arc := range arcs {
		var eps []epoch.ID
		if *ep != "" {
			id, err := epoch.Parse(*ep)
			if err != nil {
				return err
			}
			eps = []epoch.ID{id}
		} else {
			cats, err := arc.Catalogs(ctx)
			if err != nil {
				return err
			}
			for _, c := range cats {
				eps = append(eps, c.Epoch)
			}
		}
		for _, id := range eps {
			name := arc.Name + "/" + string(id)
			v, err := arc.Verify(ctx, id)
			if err != nil {
				fmt.Printf("%s: error: %v\n", name, err)
				failed++
				continue
			}
			if v.OK {
				fmt.Printf("%s: ok (%d captures, %d indexed records, %d records read)\n", name, v.Captures, v.Records, v.Scanned)
				continue
			}
			failed++
			fmt.Printf("%s: %d problem(s)\n", name, len(v.Problems)+v.MoreProblems)
			for _, p := range v.Problems {
				fmt.Printf("  %s: %s\n", p.Path, p.Message)
			}
		}
	}
	if failed > 0 {
		return exitCode(1)
	}
	return nil
}

func cmdReindex(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("reindex", flag.ContinueOnError)
	ep := epochFlag(fs)
	st := storageFlag(fs, "storage to reindex (default: all)")
	force := fs.Bool("force", false, "rebuild even epochs that look consistent")
	if err := fs.Parse(args); err != nil {
		return err
	}
	opts := archive.ReindexOptions{Force: *force}
	if *ep != "" {
		id, err := epoch.Parse(*ep)
		if err != nil {
			return err
		}
		opts.Epoch = id
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
	epochs, captures, scanned := 0, 0, 0
	var missing []string
	for _, arc := range arcs {
		opts.Progress = func(id epoch.ID, done, total int) {
			fmt.Fprintf(os.Stderr, "\r%s/%s %d/%d", arc.Name, id, done, total)
		}
		rep, err := arc.Reindex(ctx, opts)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		epochs, captures, scanned = epochs+len(rep.Epochs), captures+rep.Captures, scanned+len(rep.Scanned)
		for _, ep := range rep.Missing {
			missing = append(missing, arc.Name+"/"+string(ep))
		}
	}
	fmt.Printf("rebuilt %d epoch(s), %d open capture(s), %d by scanning WARC data\n", epochs, captures, scanned)
	if len(missing) > 0 {
		return fmt.Errorf("sealed week(s) with packages missing from storage, marked failed: %s", strings.Join(missing, ", "))
	}
	return nil
}

func cmdExport(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	page := fs.Int64("page", 0, "page id (as shown in the UI)")
	url := fs.String("url", "", "archived URL (closest capture to -at)")
	at := fs.String("at", "", "capture time for -url, RFC 3339 (default: latest)")
	out := fs.String("o", "-", "output file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	in, err := openInstance(ctx)
	if err != nil {
		return err
	}
	defer in.close()
	f := export.ArchiveFetcher{Library: in.lib}
	target := *url
	if *page != 0 {
		p, err := in.idx.Page(ctx, *page)
		if err != nil {
			return err
		}
		target, f.At, f.Storage = p.URI, p.Captured, p.Storage
	}
	if target == "" {
		return errors.New("use -page or -url")
	}
	if *at != "" {
		t, err := time.Parse(time.RFC3339, *at)
		if err != nil {
			return err
		}
		f.At = t
	}
	html, err := export.Page(ctx, f, target, export.Options{Comment: "Archived copy of " + target + " (hoarfrost)"})
	if err != nil {
		return err
	}
	if *out == "-" {
		_, err = os.Stdout.Write(html)
		return err
	}
	return os.WriteFile(*out, html, 0o644)
}

func cmdImportLegacy(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import-legacy", flag.ContinueOnError)
	date := fs.String("date", "", "capture date for all files, YYYY-MM-DD (default: file modification time)")
	batch := fs.Int("batch", 100, "pages per WARC file")
	st := storageFlag(fs, "storage receiving the pages (default: the default storage)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: hoarfrost import-legacy [flags] <directory>")
	}
	opts := legacy.Options{Storage: *st, BatchSize: *batch, Progress: func(done, total int) { fmt.Fprintf(os.Stderr, "\r%d/%d", done, total) }}
	if *date != "" {
		t, err := time.Parse(time.DateOnly, *date)
		if err != nil {
			return err
		}
		opts.Date = t
	}
	in, err := openInstance(ctx)
	if err != nil {
		return err
	}
	defer in.close()
	arc, err := in.lib.Archive(*st)
	if err != nil {
		return err
	}
	opts.Writable = func(ctx context.Context, ep epoch.ID) bool {
		info, err := in.idx.Epoch(ctx, arc.Name, ep)
		if err != nil || info.State != index.StateOpen {
			return false
		}
		_, err = arc.ReadCatalog(ctx, ep)
		return errors.Is(err, storage.ErrNotExist)
	}
	rep, err := legacy.Import(ctx, in.coord, fs.Arg(0), opts)
	fmt.Fprintln(os.Stderr)
	if rep != nil {
		fmt.Printf("imported %d page(s) into %d file(s)\n", rep.Pages, rep.Files)
		for _, e := range rep.Errors {
			fmt.Println("  error:", e)
		}
	}
	return err
}

func cmdCleanup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("cleanup", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	in, err := openInstance(ctx)
	if err != nil {
		return err
	}
	defer in.close()
	total := 0
	for _, arc := range in.lib.All() {
		n, err := arc.Cleanup(ctx)
		total += n
		if err != nil {
			return err
		}
	}
	fmt.Printf("removed %d incoming file(s)\n", total)
	return nil
}

func cmdStats(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	ep := epochFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	in, err := openInstance(ctx)
	if err != nil {
		return err
	}
	defer in.close()
	rows, err := in.idx.Volume(ctx, epoch.ID(*ep))
	if err != nil {
		return err
	}
	st, err := in.idx.Stats(ctx, 0)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "CATEGORY\tRECORDS\tREVISITS\tSTORED BYTES\t")
	var total int64
	for _, r := range rows {
		total += r.Bytes
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t\n", r.Category, r.Records, r.Revisits, r.Bytes)
	}
	fmt.Fprintf(tw, "total\t\t\t%d\t\n", total)
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\npages %d, revisits %d, saved by deduplication %d bytes\n", st.Pages, st.Revisits, st.DedupSaved)
	for _, e := range st.EpochSizes {
		fmt.Printf("%s  %-10s %-9s %6d pages  %d bytes\n", e.Epoch, e.Storage, e.State, e.Pages, e.Bytes)
	}
	return nil
}
