package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cliFlagSet(t *testing.T, args []string) (*flag.FlagSet, *cliValues) {
	t.Helper()
	fs := flag.NewFlagSet("kubeflow-admin", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	values := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("the command line %v was rejected: %v", args, err)
	}
	return fs, values
}

// A recovery flag that was given at all selects the offline reconciliation
// mode, so it needs the ID of one operation. Without it the process must
// refuse instead of starting a writable server.
func TestCLIModeRefusesARecoveryFlagWithoutAnOperationID(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{"empty id alone", []string{"-reconcile-profile-delete="}},
		{"empty id with a confirmation and a reason", []string{"-reconcile-profile-delete=", "-confirm-profile", "alice", "-reason", "verified"}},
		{"empty id with a confirmation", []string{"-reconcile-profile-delete=", "-confirm-profile", "alice"}},
		{"confirmation only", []string{"-confirm-profile", "alice"}},
		{"reason only", []string{"-reason", "verified"}},
		{"apply only", []string{"-apply"}},
		{"apply false only", []string{"-apply=false"}},
		{"whitespace id", []string{"-reconcile-profile-delete", "   "}},
		{"whitespace id with a confirmation", []string{"-reconcile-profile-delete", "\t ", "-confirm-profile", "alice"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fs, values := cliFlagSet(t, test.args)
			mode, err := selectMode(fs, values)
			if err == nil {
				t.Fatalf("a recovery flag without an operation ID selected the mode %v", mode.Mode)
			}
			if mode.Mode != cliModeUnresolved {
				t.Fatalf("a refused command line selected the mode %v", mode.Mode)
			}
		})
	}
}

// -apply without a reconciliation target keeps its own clear error.
func TestCLIModeRefusesApplyWithoutAReconciliationTarget(t *testing.T) {
	fs, values := cliFlagSet(t, []string{"-apply"})
	_, err := selectMode(fs, values)
	if err == nil {
		t.Fatal("-apply without a reconciliation target was accepted")
	}
	if !strings.Contains(err.Error(), "-apply is only valid together with -reconcile-profile-delete") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCLIModeAcceptsTheOrdinaryModes(t *testing.T) {
	fs, values := cliFlagSet(t, []string{"-config", "/etc/kubeflow-admin/config.yaml"})
	mode, err := selectMode(fs, values)
	if err != nil {
		t.Fatal(err)
	}
	if mode.Mode != cliModeServer {
		t.Fatalf("the server mode was not selected: %v", mode.Mode)
	}
	fs, values = cliFlagSet(t, []string{"-check-db"})
	mode, err = selectMode(fs, values)
	if err != nil {
		t.Fatal(err)
	}
	if mode.Mode != cliModeCheckDB {
		t.Fatalf("the database check mode was not selected: %v", mode.Mode)
	}
	fs, values = cliFlagSet(t, []string{"-check-db", "-hold", "5s"})
	mode, err = selectMode(fs, values)
	if err != nil {
		t.Fatal(err)
	}
	if mode.Mode != cliModeCheckDB || mode.Hold != 5*time.Second {
		t.Fatalf("the database check mode was not selected: %v %v", mode.Mode, mode.Hold)
	}
}

func TestCLIModeAcceptsTheFullRecoverySet(t *testing.T) {
	fs, values := cliFlagSet(t, []string{"-reconcile-profile-delete", "op-123", "-confirm-profile", "alice", "-reason", "verified", "-apply"})
	mode, err := selectMode(fs, values)
	if err != nil {
		t.Fatal(err)
	}
	if mode.Mode != cliModeReconcile {
		t.Fatalf("the reconciliation mode was not selected: %v", mode.Mode)
	}
	if mode.Reconcile.OperationID != "op-123" || mode.Reconcile.ConfirmProfile != "alice" || mode.Reconcile.Reason != "verified" || !mode.Reconcile.Apply {
		t.Fatalf("the reconciliation values are wrong: %+v", mode.Reconcile)
	}
}

// The operation ID and the confirmation are validated but never rewritten:
// the store looks up exactly what the operator typed and the confirmation is
// compared exactly.
func TestCLIModeKeepsTheTypedValuesUnchanged(t *testing.T) {
	fs, values := cliFlagSet(t, []string{"-reconcile-profile-delete", " op-123 ", "-confirm-profile", " alice "})
	mode, err := selectMode(fs, values)
	if err != nil {
		t.Fatal(err)
	}
	if mode.Reconcile.OperationID != " op-123 " {
		t.Fatalf("the operation ID was rewritten: %q", mode.Reconcile.OperationID)
	}
	if mode.Reconcile.ConfirmProfile != " alice " {
		t.Fatalf("the confirmation was rewritten: %q", mode.Reconcile.ConfirmProfile)
	}
}

// The reconciliation and the database check use the same database in
// incompatible ways: a contradictory command line is refused instead of
// silently ignoring the maintenance intent.
func TestCLIModeRefusesTheReconciliationCombinedWithTheDatabaseCheck(t *testing.T) {
	for _, args := range [][]string{
		{"-reconcile-profile-delete", "op-123", "-check-db"},
		{"-reconcile-profile-delete", "op-123", "-check-db=false"},
		{"-reconcile-profile-delete", "op-123", "-hold", "5s"},
	} {
		fs, values := cliFlagSet(t, args)
		mode, err := selectMode(fs, values)
		if err == nil {
			t.Fatalf("the contradictory command line %v selected the mode %v", args, mode.Mode)
		}
	}
}

// The refusal happens before the configuration is read, so a missing
// configuration file is never the reported cause.
func TestCLIRefusedModeIsRefusedBeforeTheConfigurationIsRead(t *testing.T) {
	fs := flag.NewFlagSet("kubeflow-admin", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	_, _, err := cliPrepare(fs, []string{"-config", filepath.Join(t.TempDir(), "absent.yaml"), "-confirm-profile", "alice"})
	if err == nil {
		t.Fatal("a recovery flag without an operation ID was accepted")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the configuration was read before the mode was refused: %v", err)
	}
}
