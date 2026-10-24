package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/ogefest/hoarfrost/internal/appdb"
	"github.com/ogefest/hoarfrost/internal/archive"
	"github.com/ogefest/hoarfrost/internal/auth"
	"github.com/ogefest/hoarfrost/internal/config"
	"github.com/ogefest/hoarfrost/internal/coordinator"
	"github.com/ogefest/hoarfrost/internal/engine"
	"github.com/ogefest/hoarfrost/internal/engine/chrome"
	"github.com/ogefest/hoarfrost/internal/index"
	"github.com/ogefest/hoarfrost/internal/queue"
	"github.com/ogefest/hoarfrost/internal/sources"
	"github.com/ogefest/hoarfrost/internal/sourcestore"
	"github.com/ogefest/hoarfrost/internal/storage"
	"github.com/ogefest/hoarfrost/internal/worker"
)

func newLogger(env *config.Env) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(env.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if env.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

// openStorages opens every configured storage with its optional replica.
func openStorages(ctx context.Context, env *config.Env, log *slog.Logger) (map[string]storage.Storage, func(), error) {
	out := map[string]storage.Storage{}
	var closers []func()
	closeAll := func() {
		for _, c := range closers {
			c()
		}
	}
	for _, name := range env.StorageNames() {
		st, err := storage.OpenNamed(ctx, name, env.Storages[name])
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("storage %s: %w", name, err)
		}
		if rep := env.Replicas[name]; rep != "" {
			sec, err := storage.OpenNamed(ctx, name, rep)
			if err != nil {
				closeAll()
				return nil, nil, fmt.Errorf("replica of %s: %w", name, err)
			}
			r := storage.NewReplica(st, sec, log)
			closers = append(closers, func() { r.Close() })
			st = r
		}
		out[name] = st
	}
	return out, closeAll, nil
}

// instance holds everything a coordinator-side command needs.
type instance struct {
	env     *config.Env
	log     *slog.Logger
	stores  map[string]storage.Storage
	idx     *index.DB
	db      *sql.DB
	lib     *archive.Library
	queue   *queue.Queue
	coord   *coordinator.Coordinator
	auth    *auth.Store
	closers []func()
}

func openInstance(ctx context.Context) (*instance, error) {
	env, err := config.FromEnvironment()
	if err != nil {
		return nil, err
	}
	in := &instance{env: env, log: newLogger(env)}
	if err := os.MkdirAll(env.DataDir, 0o750); err != nil {
		return nil, err
	}
	stores, closeStores, err := openStorages(ctx, env, in.log)
	if err != nil {
		return nil, err
	}
	in.stores = stores
	in.closers = append(in.closers, closeStores)
	if in.idx, err = index.Open(ctx, env.Database, index.Schema); err != nil {
		in.close()
		return nil, err
	}
	in.closers = append(in.closers, func() { in.idx.Close() })
	if in.db, err = appdb.Open(ctx, env.Database, appdb.Schema); err != nil {
		in.close()
		return nil, err
	}
	in.closers = append(in.closers, func() { in.db.Close() })
	in.lib = archive.NewLibrary(in.idx, stores, filepath.Join(env.DataDir, "spool"), in.log)
	for _, a := range in.lib.All() {
		a.IncomingRetention = env.Retention
		a.ConcatBySource = env.ConcatBySource
	}
	in.closers = append(in.closers, in.lib.Close)
	if os.Getenv("HOARFROST_SOURCES") != "" {
		in.log.Warn("HOARFROST_SOURCES is no longer used: sources are kept in the database and managed in the web UI (Sources) or through /api/sources")
	}
	in.queue = queue.New(in.db)
	in.coord, err = coordinator.New(coordinator.Options{
		Library: in.lib, Queue: in.queue, Fetcher: sources.NewFetcher(env.UserAgent), Sources: sourcestore.New(in.db),
		LeaseDuration: env.LeaseDuration, SealGrace: env.SealGrace, Log: in.log,
	})
	if err != nil {
		in.close()
		return nil, err
	}
	in.auth = auth.New(in.db, env.Secret)
	return in, nil
}

// archives returns the archive of one storage, or all when name is empty.
func (in *instance) archives(name string) ([]*archive.Archive, error) {
	if name == "" {
		return in.lib.All(), nil
	}
	a, err := in.lib.Archive(name)
	if err != nil {
		return nil, err
	}
	return []*archive.Archive{a}, nil
}

// descriptions lists storages for the system page, without credentials.
func (in *instance) descriptions() []string {
	var out []string
	for _, name := range in.lib.Names() {
		out = append(out, name+": "+in.stores[name].String())
	}
	return out
}

func (in *instance) close() {
	for i := len(in.closers) - 1; i >= 0; i-- {
		in.closers[i]()
	}
}

// engines returns the capture engines available on this machine.
func engines(env *config.Env, names string, log *slog.Logger) (engine.Registry, func()) {
	reg := worker.DefaultEngines()
	closer := func() {}
	if chrome.Available(env.ChromePath) {
		c := &chrome.Engine{Bin: env.ChromePath, NoSandbox: env.ChromeNoSandbox || os.Getuid() == 0}
		reg[engine.Chrome] = c
		closer = func() { c.Close() }
	} else {
		log.Info("no browser found, chrome engine disabled (set HOARFROST_CHROME)")
	}
	if names != "" {
		keep := map[string]bool{}
		for _, n := range strings.Split(names, ",") {
			keep[strings.TrimSpace(n)] = true
		}
		for n := range reg {
			if !keep[n] {
				delete(reg, n)
			}
		}
	}
	return reg, closer
}
