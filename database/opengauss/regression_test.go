package opengauss

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

func TestInitializationLockUsesUpstreamTimeout(t *testing.T) {
	previous := migrate.DefaultLockTimeout
	migrate.DefaultLockTimeout = 5 * time.Millisecond
	t.Cleanup(func() { migrate.DefaultLockTimeout = previous })
	db, mock := mockDB(t)
	expectMetadata(mock, "A", 0)
	expectLock(mock, false)
	mock.ExpectClose()
	if _, err := WithInstance(db, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("initialization did not honor lock timeout: %v", err)
	}
}

func TestUpstreamDoesNotMarkUncommittedMigrationClean(t *testing.T) {
	g, mock := initializedDriver(t)
	script := "BEGIN; INSERT INTO t VALUES(1);"
	source, err := iofs.New(fstest.MapFS{"1_probe.up.sql": &fstest.MapFile{Data: []byte(script)}}, ".")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	m, err := migrate.NewWithInstance("iofs", source, "opengauss", g)
	if err != nil {
		t.Fatal(err)
	}
	expectLock(mock, true)
	mock.ExpectQuery(`SELECT "version", "dirty"`).WillReturnRows(versionRows())
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO`).WithArgs(1, 1).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	expectRun(mock, regexp.QuoteMeta(script)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(transactionProbeSQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	expectRollback(mock)
	expectUnlock(mock, true)
	if err := m.Up(); err == nil || !strings.Contains(err.Error(), "open transaction") {
		t.Fatalf("uncommitted migration = %v", err)
	}
}

func TestRunRejectsUncertainTransactionProbe(t *testing.T) {
	for _, failure := range []error{errors.New("probe denied"), context.DeadlineExceeded} {
		t.Run(failure.Error(), func(t *testing.T) {
			g, mock := initializedDriver(t)
			expectRun(mock, `SELECT 1`).WillReturnResult(sqlmock.NewResult(0, 0))
			mock.ExpectExec(regexp.QuoteMeta(transactionProbeSQL)).WillReturnError(failure)
			fatal := errors.Is(failure, context.DeadlineExceeded)
			if fatal {
				mock.ExpectClose()
			} else {
				expectRollback(mock)
			}
			if err := g.Run(strings.NewReader("SELECT 1")); err == nil || !strings.Contains(err.Error(), failure.Error()) || g.broken.Load() != fatal {
				t.Fatalf("transaction probe = %v, broken=%v", err, g.broken.Load())
			}
		})
	}
}

func TestRunAcceptsCommittedTransactions(t *testing.T) {
	g, mock := initializedDriver(t)
	script := committedScript
	expectRun(mock, regexp.QuoteMeta(script)).WillReturnResult(sqlmock.NewResult(0, 0))
	expectIdleProbe(mock)
	if err := g.Run(strings.NewReader(script)); err != nil {
		t.Fatal(err)
	}
}

func TestInitializationRejectsNonPermanentState(t *testing.T) {
	for _, persistence := range []string{"u", "t", "g"} {
		t.Run(persistence, func(t *testing.T) {
			db, mock := mockDB(t)
			expectMetadata(mock, "A", 0)
			expectLock(mock, true)
			mock.ExpectQuery(`SELECT c.relkind`).WithArgs(int64(42), "schema_migrations").
				WillReturnRows(sqlmock.NewRows(relationColumns).AddRow("r", persistence))
			expectUnlock(mock, true)
			mock.ExpectClose()
			if _, err := WithInstance(db, nil); err == nil || !strings.Contains(err.Error(), "permanent") {
				t.Fatalf("non-permanent migration state = %v", err)
			}
		})
	}
}
