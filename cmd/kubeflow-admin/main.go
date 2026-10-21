package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"kubeflow-admin/internal/auth"
	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/kube"
	"kubeflow-admin/internal/logging"
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
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	// The mode is resolved before the configuration is loaded, before the
	// database is opened and before the server starts. A refused mode never
	// reaches any of them.
	mode, cfg, err := cliPrepare(flag.CommandLine, os.Args[1:])
	if err != nil {
		return err
	}
	switch mode.Mode {
	case cliModeReconcile:
		// The offline reconciliation is a separate mode: it never starts the
		// server and it never serves a request.
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer cancel()
		ctx, cancel = context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		return reconcileProfileDelete(ctx, cfg, mode.Reconcile)
	case cliModeCheckDB:
		return checkDatabase(cfg, mode.Hold)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	// Keep database, journal and lock files private regardless of tooling.
	syscall.Umask(0077)
	logOutput, err := logging.Setup(cfg.Logging)
	if err != nil {
		return err
	}
	defer func() {
		// Keep terminal startup/shutdown errors visible after closing a file sink.
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
		if err := logOutput.Close(); err != nil {
			slog.Error("log output close failed", "error", err)
		}
	}()
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

// The names of the command line flags. They are constants so the mode
// resolution can recognise a flag the operator gave explicitly.
const (
	configFlagName         = "config"
	checkDBFlagName        = "check-db"
	holdFlagName           = "hold"
	reconcileIDFlagName    = "reconcile-profile-delete"
	confirmProfileFlagName = "confirm-profile"
	reasonFlagName         = "reason"
	applyFlagName          = "apply"
)

// cliValues are the parsed command line values of the application.
type cliValues struct {
	// ConfigPath is the configuration file to load.
	ConfigPath string
	// CheckDB requests the database ownership check.
	CheckDB bool
	// Hold keeps the database open for that long during the check.
	Hold time.Duration
	// ReconcileID is the ID of the one uncertain Profile deletion to resolve.
	ReconcileID string
	// ConfirmProfile is the exact Profile name the operator typed back.
	ConfirmProfile string
	// Reason is the offline reconciliation reason recorded in the audit.
	Reason string
	// Apply makes the derived outcome durable.
	Apply bool
}

// cliMode is the mode the command line selected.
type cliMode int

const (
	// cliModeUnresolved is the zero value: no mode was resolved.
	cliModeUnresolved cliMode = iota
	// cliModeServer starts the HTTP server.
	cliModeServer
	// cliModeCheckDB opens the database, reports its ownership and exits.
	cliModeCheckDB
	// cliModeReconcile resolves one uncertain Profile deletion offline.
	cliModeReconcile
)

// cliSelection is the resolved mode and the values that mode needs.
type cliSelection struct {
	// Mode is the selected mode.
	Mode cliMode
	// Hold is the database check hold duration.
	Hold time.Duration
	// Reconcile carries the values of the offline reconciliation.
	Reconcile profileReconcileOptions
}

// registerCLIFlags declares the command line of the application and returns
// the values the flags write into.
func registerCLIFlags(fs *flag.FlagSet) *cliValues {
	v := &cliValues{}
	fs.StringVar(&v.ConfigPath, configFlagName, "/etc/kubeflow-admin/config.yaml", "configuration YAML")
	fs.BoolVar(&v.CheckDB, checkDBFlagName, false, "open the configured database, report ownership/journal mode, then exit (exit code 3 = owned by another process)")
	fs.DurationVar(&v.Hold, holdFlagName, 0, "with -check-db: keep the database open for this duration (for cross-node ownership tests)")
	fs.StringVar(&v.ReconcileID, reconcileIDFlagName, "", "offline reconciliation mode: the ID of one uncertain profile.delete operation to resolve")
	fs.StringVar(&v.ConfirmProfile, confirmProfileFlagName, "", "with -reconcile-profile-delete: the exact Profile name typed back to confirm the target")
	fs.StringVar(&v.Reason, reasonFlagName, "", "with -reconcile-profile-delete: the offline reconciliation reason recorded in the audit")
	fs.BoolVar(&v.Apply, applyFlagName, false, "with -reconcile-profile-delete: record the derived outcome; without it the decision is only reported")
	return v
}

// selectMode decides which mode the command line selects.
//
// It is a pure function of the parsed values and of the flags the operator
// gave explicitly, so it can be exercised without a configuration, a
// database or a server.
//
// A recovery flag that was given at all is an explicit request for the
// offline reconciliation mode, even when its value is empty or false. Such a
// request without a usable operation ID is refused here: it must never fall
// through to the writable server mode, because an operator who believes the
// process is reconciling an operation must not end up serving requests
// instead.
func selectMode(fs *flag.FlagSet, v *cliValues) (cliSelection, error) {
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	recovery := given[reconcileIDFlagName] || given[confirmProfileFlagName] || given[reasonFlagName] || given[applyFlagName]
	// The operation ID is validated but never rewritten: the store looks up
	// exactly what the operator typed.
	id := v.ReconcileID
	if recovery && strings.TrimSpace(id) == "" {
		if given[applyFlagName] && v.Apply {
			return cliSelection{}, errors.New("-apply is only valid together with -reconcile-profile-delete, which requires the ID of one operation to reconcile")
		}
		return cliSelection{}, errors.New("the offline reconciliation requires the ID of one operation to reconcile: pass -reconcile-profile-delete=OPERATION_ID")
	}
	if strings.TrimSpace(id) == "" {
		// No recovery flag was given, so the ordinary modes are unchanged.
		if v.Apply {
			// -apply only means something together with a reconciliation
			// target. Never let it look like a general write switch.
			return cliSelection{}, errors.New("-apply is only valid together with -reconcile-profile-delete")
		}
		if v.CheckDB {
			return cliSelection{Mode: cliModeCheckDB, Hold: v.Hold}, nil
		}
		return cliSelection{Mode: cliModeServer}, nil
	}
	// The reconciliation and the database check use the same database in
	// incompatible ways. A contradictory command line is refused instead of
	// silently ignoring the maintenance intent.
	if given[checkDBFlagName] {
		return cliSelection{}, errors.New("the offline reconciliation cannot be combined with -check-db")
	}
	if given[holdFlagName] {
		return cliSelection{}, errors.New("the offline reconciliation cannot be combined with -hold: -hold belongs to the -check-db mode")
	}
	return cliSelection{Mode: cliModeReconcile, Reconcile: profileReconcileOptions{OperationID: id, ConfirmProfile: v.ConfirmProfile, Reason: v.Reason, Apply: v.Apply}}, nil
}

// cliPrepare parses the command line, resolves the mode and loads the
// configuration.
//
// The mode is resolved first, so a refused mode never loads a configuration,
// never opens a database and never starts a server.
func cliPrepare(fs *flag.FlagSet, args []string) (cliSelection, config.Config, error) {
	values := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return cliSelection{}, config.Config{}, err
	}
	mode, err := selectMode(fs, values)
	if err != nil {
		return cliSelection{}, config.Config{}, err
	}
	cfg, err := config.Load(values.ConfigPath)
	if err != nil {
		return cliSelection{}, config.Config{}, err
	}
	return mode, cfg, nil
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
