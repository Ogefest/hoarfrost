package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/ogefest/hoarfrost/internal/web"
	"github.com/ogefest/hoarfrost/internal/worker"
)

func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	engineNames := fs.String("engines", "", "comma-separated engines for the embedded worker (default: all available)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	in, err := openInstance(ctx)
	if err != nil {
		return err
	}
	defer in.close()
	env := in.env
	if err := env.RequireSecret(); err != nil {
		return err
	}
	log := in.log
	log.Info("starting coordinator", "storages", in.descriptions(), "data", env.DataDir, "listen", env.Listen)
	if err := in.coord.Start(ctx); err != nil {
		return err
	}
	if n, err := in.auth.CountUsers(ctx); err == nil && n == 0 {
		log.Warn("no users yet: open the web UI and create the administrator at /setup")
	}

	var wg sync.WaitGroup
	var active func() []string
	if env.EmbeddedWorker && env.Workers > 0 {
		reg, closeEngines := engines(env, *engineNames, log)
		defer closeEngines()
		w := &worker.Worker{Name: env.WorkerName + "/embedded", Coord: in.coord, Storages: in.stores, Engines: reg,
			SpoolDir: filepath.Join(env.DataDir, "spool"), Slots: env.Workers, Log: log}
		active = w.Active
		wg.Add(1)
		go func() { defer wg.Done(); _ = w.Run(ctx) }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); in.coord.Run(ctx, 30*time.Second) }()

	srv, err := web.New(&web.Server{
		Coord: in.coord, Auth: in.auth, Log: log, SecureCookie: !env.InsecureCookie, WorkerToken: env.WorkerToken,
		Active: active, Storage: in.descriptions(),
	})
	if err != nil {
		return err
	}
	hs := &http.Server{Addr: env.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	log.Info("web UI listening", "addr", env.Listen)
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil {
		log.Warn("shutdown", "err", err)
	}
	wg.Wait()
	return nil
}
