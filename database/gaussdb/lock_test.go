package gaussdb

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

func TestUpstreamLockTimeoutCloseCancelsAcquisition(t *testing.T) {
	g, mock := initializedDriver(t)
	source, err := iofs.New(fstest.MapFS{"1_probe.up.sql": &fstest.MapFile{Data: []byte("SELECT 1;")}}, ".")
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.NewWithInstance("iofs", source, "gaussdb", g)
	if err != nil {
		t.Fatal(err)
	}
	m.LockTimeout = 10 * time.Millisecond
	expectLock(mock, false)
	mock.ExpectClose()
	if err := m.Up(); !errors.Is(err, migrate.ErrLockTimeout) {
		t.Fatalf("lock timeout = %v", err)
	}
	if sourceErr, dbErr := m.Close(); sourceErr != nil || dbErr != nil {
		t.Fatalf("close pending lock: %v, %v", sourceErr, dbErr)
	}
	if err := g.Lock(); !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("reused closed migrator: %v", err)
	}
}

type startedLockArgument chan struct{}

func (started startedLockArgument) Match(driver.Value) bool {
	close(started)
	return true
}

func TestCloseCancelsInFlightLockQuery(t *testing.T) {
	g, mock := initializedDriver(t)
	started := make(startedLockArgument)
	mock.ExpectQuery(`SELECT pg_catalog.pg_try_advisory_lock`).WithArgs(started).
		WillDelayFor(5 * time.Second).WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	mock.ExpectClose()
	result := make(chan error, 1)
	go func() { result <- g.Lock() }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("lock request did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- g.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel native lock request")
	}
	if err := <-result; err == nil {
		t.Fatal("cancelled lock acquisition succeeded")
	}
}
