package dm

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"testing/fstest"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

func TestLockRetriesContention(t *testing.T) {
	d, mock := initializedDriver(t)
	expectLock(mock, 1)
	expectLock(mock, 0)
	if err := d.Lock(); err != nil {
		t.Fatal(err)
	}
	expectUnlock(mock, 0)
	mock.ExpectClose()
}

func TestMigrateLockTimeoutAndCloseCancelPendingLock(t *testing.T) {
	d, mock := initializedDriver(t)
	source, err := iofs.New(fstest.MapFS{"1_probe.up.sql": &fstest.MapFile{Data: []byte("SELECT 1;")}}, ".")
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.NewWithInstance("iofs", source, "dm", d)
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
	if err := d.Lock(); !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("lock after close = %v", err)
	}
}

type startedLockArgument chan struct{}

func (started startedLockArgument) Match(driver.Value) bool {
	close(started)
	return true
}

func TestCloseCancelsInFlightLockRequest(t *testing.T) {
	d, mock := initializedDriver(t)
	started := make(startedLockArgument)
	mock.ExpectExec(`BEGIN \? := DBMS_LOCK.REQUEST`).WithArgs(started, sqlmock.AnyArg()).
		WillDelayFor(5 * time.Second).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectClose()
	result := make(chan error, 1)
	go func() { result <- d.Lock() }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("lock request did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- d.Close() }()
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
