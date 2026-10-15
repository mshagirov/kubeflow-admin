package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"kubeflow-admin/internal/auth"
	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/kube"
	"kubeflow-admin/internal/operations"
	"kubeflow-admin/internal/store"
	"kubeflow-admin/internal/web"
)

func main() {
	if err := run(); err != nil {
		slog.Error("application stopped", "error", err)
		if errors.Is(err, store.ErrLocked) {
			// Distinct code for the ownership check: another process owns the DB.
			os.Exit(3)
		}
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("config", "/etc/kubeflow-admin/config.yaml", "configuration YAML")
	check := flag.Bool("check-db", false, "open the configured database, report ownership/journal mode, then exit (exit code 3 = owned by another process)")
	hold := flag.Duration("hold", 0, "with -check-db: keep the database open for this duration (for cross-node ownership tests)")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	if *check {
		return checkDatabase(cfg, *hold)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	// Keep database, journal and lock files private regardless of tooling.
	syscall.Umask(0077)
	s, err := store.OpenWithJournal(cfg.Database.Path, cfg.Database.JournalMode, cfg.Database.AllowUnverifiedFilesystem)
	if err != nil {
		return err
	}
	defer s.Close()
	if s.OnUnverifiedFilesystem() {
		slog.Warn("database storage is not verifiably local; single-owner safety depends on effective client locking and remote fsync", "path", cfg.Database.Path, "journalMode", cfg.Database.JournalMode)
	}
	k, err := kube.New(cfg)
	if err != nil {
		return err
	}
	initCtx, initCancel := context.WithTimeout(ctx, 30*time.Second)
	defer initCancel()
	a, err := auth.New(initCtx, cfg, s)
	if err != nil {
		return err
	}
	m, err := operations.New(ctx, cfg, s, k)
	if err != nil {
		return err
	}
	defer m.Close()
	srv := &http.Server{Addr: cfg.Server.Listen, Handler: web.New(cfg, a, k, m, s), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	result := make(chan error, 1)
	go func() {
		slog.Info("server listening", "address", cfg.Server.Listen, "writesEnabled", cfg.Features.WritesEnabled)
		result <- srv.ListenAndServe()
	}()
	select {
	case err = <-result:
		if err != http.ErrServerClosed {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, stop := context.WithTimeout(context.Background(), 20*time.Second)
	defer stop()
	return srv.Shutdown(shutdown)
}

// checkDatabase verifies that the configured database can be opened and reports
// durability-relevant status. It backs the one-time cross-node ownership test:
// while a holder pod keeps the database open on another node, a second check
// must exit with store.ErrLocked. With -hold it keeps the exclusive lock for
// the given duration so such a test is possible with distroless images.
func checkDatabase(cfg config.Config, hold time.Duration) error {
	syscall.Umask(0077)
	s, err := store.OpenWithJournal(cfg.Database.Path, cfg.Database.JournalMode, cfg.Database.AllowUnverifiedFilesystem)
	if err != nil {
		return err
	}
	defer s.Close()
	journal := cfg.Database.JournalMode
	if journal == "" {
		journal = "wal"
	}
	fields := []any{"path", cfg.Database.Path, "journalMode", journal, "unverifiedFilesystem", s.OnUnverifiedFilesystem()}
	if hold > 0 {
		slog.Info("database check passed; holding exclusive ownership", append(fields, "hold", hold.String())...)
		time.Sleep(hold)
		slog.Info("database ownership released", "path", cfg.Database.Path)
		return nil
	}
	slog.Info("database check passed", fields...)
	return nil
}
