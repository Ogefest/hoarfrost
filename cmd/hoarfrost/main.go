// Command hoarfrost archives press articles and web pages into WARC/WACZ files.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ogefest/hoarfrost/internal/config"
	"github.com/ogefest/hoarfrost/internal/version"
)

type command struct {
	name  string
	usage string
	run   func(ctx context.Context, args []string) error
}

var commands = []command{
	{"serve", "run the coordinator with the web UI (and an embedded worker)", cmdServe},
	{"worker", "run an additional capture worker against a coordinator", cmdWorker},
	{"user", "manage users: add, list, delete, reset-password, role, token", cmdUser},
	{"seal", "seal an epoch into a weekly WACZ package", cmdSeal},
	{"verify", "verify sealed packages against their hashes and indexes", cmdVerify},
	{"reindex", "rebuild the index from storage", cmdReindex},
	{"import-sqlite", "copy users, queue and sources from an app.sqlite of an older version", cmdImportSQLite},
	{"export", "export an archived page as a single HTML file", cmdExport},
	{"import-legacy", "import a directory of single-file HTML pages", cmdImportLegacy},
	{"cleanup", "remove packaged incoming files after the retention period", cmdCleanup},
	{"stats", "show stored volume by content type and deduplication savings", cmdStats},
	{"version", "print the version", func(context.Context, []string) error { fmt.Println(version.Software()); return nil }},
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: hoarfrost [-env FILE] <command> [flags]\n\nCommands:\n")
	for _, c := range commands {
		fmt.Fprintf(os.Stderr, "  %-14s %s\n", c.name, c.usage)
	}
	fmt.Fprintf(os.Stderr, "\nConfiguration comes from HOARFROST_* environment variables and the .env file.\nRun 'hoarfrost <command> -h' for command flags.\n")
}

func main() { os.Exit(run()) }

func run() int {
	fs := flag.NewFlagSet("hoarfrost", flag.ExitOnError)
	envFile := fs.String("env", ".env", "path of the .env file")
	fs.Usage = usage
	_ = fs.Parse(os.Args[1:])
	args := fs.Args()
	if len(args) == 0 {
		usage()
		return 2
	}
	if err := config.LoadDotEnv(*envFile); err != nil {
		fmt.Fprintln(os.Stderr, "hoarfrost:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for _, c := range commands {
		if c.name != args[0] {
			continue
		}
		err := c.run(ctx, args[1:])
		if err == nil || errors.Is(err, flag.ErrHelp) {
			return 0
		}
		var ec exitCode
		if errors.As(err, &ec) {
			return int(ec)
		}
		fmt.Fprintln(os.Stderr, "hoarfrost:", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "hoarfrost: unknown command %q\n\n", args[0])
	usage()
	return 2
}

// exitCode carries a specific process exit status.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
