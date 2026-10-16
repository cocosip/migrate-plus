package dm

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"testing"
	"testing/fstest"
	"time"

	dmclient "gitee.com/chunanyong/dm"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
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
	mock.ExpectQuery(`SELECT DBMS_LOCK.REQUEST`).WithArgs(started).
		WillDelayFor(5 * time.Second).WillReturnRows(sqlmock.NewRows([]string{codeColumn}).AddRow(0))
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

func TestLockQueryFailureNeverAcquiresOrReplacesSession(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rows  *sqlmock.Rows
		err   error
		fatal bool
	}{
		{"empty", sqlmock.NewRows([]string{codeColumn}), nil, false},
		{"null", sqlmock.NewRows([]string{codeColumn}).AddRow(nil), nil, false},
		{"invalid", sqlmock.NewRows([]string{codeColumn}).AddRow("invalid"), nil, false},
		{"close", sqlmock.NewRows([]string{codeColumn}).AddRow(0).CloseError(errors.New("close failed")), nil, false},
		{"permission", nil, errors.New("permission denied"), false},
		{"disconnected", nil, &dmclient.DmError{ErrCode: 6001}, true},
		{"bad connection", nil, driver.ErrBadConn, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, mock := initializedDriver(t)
			request := mock.ExpectQuery(`^SELECT DBMS_LOCK.REQUEST\(\?, 6, 0, FALSE\) FROM DUAL$`).WithArgs(d.lockID)
			if tc.err != nil {
				request.WillReturnError(tc.err)
			} else {
				request.WillReturnRows(tc.rows).RowsWillBeClosed()
			}
			if tc.fatal {
				mock.ExpectClose()
			}
			if err := d.Lock(); err == nil || d.isLocked.Load() {
				t.Fatalf("failed request acquired lock: %v", err)
			}
			if tc.fatal {
				if err := d.Lock(); !errors.Is(err, sql.ErrConnDone) {
					t.Fatalf("replaced lost session: %v", err)
				}
				return
			}
			expectLock(mock, 0)
			if err := d.Lock(); err != nil {
				t.Fatal(err)
			}
			expectUnlock(mock, 0)
			mock.ExpectClose()
		})
	}
}

func TestUnlockQueryFailurePreservesStateAndDiscardsLostSession(t *testing.T) {
	for _, tc := range []struct {
		err   error
		fatal bool
	}{{errors.New("release denied"), false}, {&dmclient.DmError{ErrCode: 6001}, true}} {
		t.Run(fmt.Sprint(tc.err), func(t *testing.T) {
			d, mock := initializedDriver(t)
			expectLock(mock, 0)
			if err := d.Lock(); err != nil {
				t.Fatal(err)
			}
			mock.ExpectQuery(`^SELECT DBMS_LOCK.RELEASE\(\?\) FROM DUAL$`).WithArgs(d.lockID).WillReturnError(tc.err)
			if tc.fatal {
				mock.ExpectClose()
			}
			if err := d.Unlock(); err == nil || !d.isLocked.Load() {
				t.Fatalf("failed release lost lock state: %v", err)
			}
			if !tc.fatal {
				expectUnlock(mock, 0)
				if err := d.Unlock(); err != nil {
					t.Fatal(err)
				}
				mock.ExpectClose()
			} else if err := d.Lock(); !errors.Is(err, sql.ErrConnDone) {
				t.Fatalf("replaced lost session: %v", err)
			}
		})
	}
}

func TestInitializationLockDisconnectStopsBeforeVersionTable(t *testing.T) {
	db, mock := mockDB(t)
	expectMetadata(mock, 2, 1, 0)
	failure := &dmclient.DmError{ErrCode: 6001}
	mock.ExpectQuery(`SELECT DBMS_LOCK.REQUEST`).WithArgs(sqlmock.AnyArg()).WillReturnError(failure)
	mock.ExpectClose()
	_, err := WithInstance(db, nil)
	var operation database.Error
	if !errors.As(err, &operation) || operation.OrigErr != failure {
		t.Fatalf("initialization lost first lock error: %v", err)
	}
}
