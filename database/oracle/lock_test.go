package oracle

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

func TestUncertainVersionTransactionDiscardsSession(t *testing.T) {
	for _, commit := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "commit"}[commit], func(t *testing.T) {
			o, mock := initializedDriver(t)
			expectLock(mock, 0)
			if err := o.Lock(); err != nil {
				t.Fatal(err)
			}
			failure := context.DeadlineExceeded
			mock.ExpectBegin()
			if commit {
				mock.ExpectExec(`DELETE FROM`).WillReturnResult(sqlmock.NewResult(0, 1))
				mock.ExpectCommit().WillReturnError(failure)
			} else {
				mock.ExpectExec(`DELETE FROM`).WillReturnError(errors.New("delete failed"))
				mock.ExpectRollback().WillReturnError(failure)
			}
			mock.ExpectClose()
			if err := o.SetVersion(-1, false); !errors.Is(err, failure) {
				t.Fatalf("lost transaction error: %v", err)
			}
			if err := o.Run(strings.NewReader("CREATE TABLE unsafe_reuse(id NUMBER)")); !errors.Is(err, sql.ErrConnDone) {
				t.Fatalf("reused an uncertain transaction session: %v", err)
			}
			if _, _, err := o.Version(); !errors.Is(err, sql.ErrConnDone) {
				t.Fatalf("read from an uncertain transaction session: %v", err)
			}
		})
	}
}

func TestVersionTransactionTimeoutDiscardsSession(t *testing.T) {
	o, mock := initializedDriver(t)
	o.config.StatementTimeout = 10 * time.Millisecond
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM`).WillDelayFor(time.Second).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectRollback()
	mock.ExpectClose()
	if err := o.SetVersion(-1, false); err == nil {
		t.Fatal("transaction timeout was ignored")
	}
	if err := o.Run(strings.NewReader("CREATE TABLE unsafe_reuse(id NUMBER)")); !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("reused a timed out transaction session: %v", err)
	}
}

func TestLockRetriesContention(t *testing.T) {
	o, mock := initializedDriver(t)
	expectLock(mock, 1)
	expectLock(mock, 0)
	if err := o.Lock(); err != nil {
		t.Fatal(err)
	}
	expectUnlock(mock, 0)
}

func TestMigrateLockTimeoutAndCloseCancelPendingLock(t *testing.T) {
	o, mock := initializedDriver(t)
	source, err := iofs.New(fstest.MapFS{"1_probe.up.sql": &fstest.MapFile{Data: []byte("SELECT 1 FROM dual;")}}, ".")
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.NewWithInstance("iofs", source, "oracle", o)
	if err != nil {
		t.Fatal(err)
	}
	m.LockTimeout = 10 * time.Millisecond
	expectLock(mock, 1)
	mock.ExpectClose()
	if err := m.Up(); !errors.Is(err, migrate.ErrLockTimeout) {
		t.Fatalf("lock timeout = %v", err)
	}
	if sourceErr, dbErr := m.Close(); sourceErr != nil || dbErr != nil {
		t.Fatalf("close pending lock: %v, %v", sourceErr, dbErr)
	}
	if err := o.Lock(); !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("lock after close = %v", err)
	}
}

type startedLockArgument chan struct{}

func (started startedLockArgument) Match(driver.Value) bool {
	close(started)
	return true
}

func TestCloseCancelsInFlightLockRequest(t *testing.T) {
	o, mock := initializedDriver(t)
	started := make(startedLockArgument)
	mock.ExpectExec(`BEGIN :1 := DBMS_LOCK.REQUEST`).WithArgs(started, sqlmock.AnyArg()).
		WillDelayFor(5 * time.Second).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectClose()
	result := make(chan error, 1)
	go func() { result <- o.Lock() }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("lock request did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- o.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel the native request")
	}
	if err := <-result; err == nil {
		t.Fatal("cancelled lock request succeeded")
	}
}
