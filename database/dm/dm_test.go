package dm

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	dmclient "gitee.com/chunanyong/dm"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/golang-migrate/migrate/v4/database"
)

const (
	testSchema    = "APP"
	testProduct   = "DM Database Server 64 V8"
	countColumn   = "count"
	dirtyColumn   = "DIRTY"
	versionColumn = "VERSION"
	historyTable  = "HISTORY"
	historyInput  = "history"
)

type outputConverter struct{}

func (outputConverter) ConvertValue(v any) (driver.Value, error) {
	if _, ok := v.(sql.Out); ok {
		return v, nil
	}
	return driver.DefaultParameterConverter.ConvertValue(v)
}

type outputCode int

func (code outputCode) Match(v driver.Value) bool {
	out, ok := v.(sql.Out)
	if !ok {
		return false
	}
	reflect.ValueOf(out.Dest).Elem().SetInt(int64(code))
	return true
}

func mockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.ValueConverterOption(outputConverter{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		_ = db.Close()
	})
	return db, mock
}

func expectMetadata(mock sqlmock.Sqlmock, mode, caseSensitive, backslash int) {
	mock.ExpectQuery(`SELECT BANNER FROM V\$VERSION`).WillReturnRows(sqlmock.NewRows([]string{"BANNER"}).AddRow(testProduct).AddRow("Msg Version: 7.6.0.99"))
	mock.ExpectQuery(`SELECT CURRENT_SCHID\(\), SF_GET_SCHEMA_NAME_BY_ID`).WillReturnRows(
		sqlmock.NewRows([]string{"id", "schema", "mode", "case", "escape"}).AddRow(101, testSchema, mode, caseSensitive, backslash))
}

func expectLock(mock sqlmock.Sqlmock, code int) {
	mock.ExpectExec(`BEGIN \? := DBMS_LOCK.REQUEST\(\?, 6, 0, FALSE\); END;`).WithArgs(outputCode(code), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 0))
}

func expectUnlock(mock sqlmock.Sqlmock, code int) {
	mock.ExpectExec(`BEGIN \? := DBMS_LOCK.RELEASE\(\?\); END;`).WithArgs(outputCode(code), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 0))
}

func versionColumns() *sqlmock.Rows {
	return sqlmock.NewRowsWithColumnDefinition(sqlmock.NewColumn(versionColumn).OfType("BIGINT", int64(0)), sqlmock.NewColumn(dirtyColumn).OfType("SMALLINT", int64(0)))
}

func expectVersionTable(mock sqlmock.Sqlmock, exists bool) {
	count := 0
	if exists {
		count = 1
	}
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM SYS.SYSOBJECTS`).WithArgs(101, "SCHEMA_MIGRATIONS").WillReturnRows(sqlmock.NewRows([]string{countColumn}).AddRow(count))
	if !exists {
		mock.ExpectExec(`CREATE TABLE "APP"\."SCHEMA_MIGRATIONS" \("VERSION" BIGINT NOT NULL PRIMARY KEY, "DIRTY" SMALLINT NOT NULL CHECK \("DIRTY" IN \(0, 1\)\)\)`).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	mock.ExpectQuery(`SELECT "VERSION", "DIRTY" FROM "APP"\."SCHEMA_MIGRATIONS" WHERE 1 = 0`).WillReturnRows(versionColumns())
	mock.ExpectQuery(`SELECT "VERSION", "DIRTY" FROM "APP"\."SCHEMA_MIGRATIONS"$`).WillReturnRows(sqlmock.NewRows([]string{versionColumn, dirtyColumn}))
}

func initializedDriver(t *testing.T) (*Dm, sqlmock.Sqlmock) {
	t.Helper()
	db, mock := mockDB(t)
	expectMetadata(mock, 0, 1, 0)
	expectLock(mock, 0)
	expectVersionTable(mock, true)
	expectUnlock(mock, 0)
	d, err := WithInstance(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d.(*Dm), mock
}

func TestWithInstanceUsesActualBackslashSetting(t *testing.T) {
	db, mock := mockDB(t)
	expectMetadata(mock, 7, 1, 1)
	expectLock(mock, 0)
	expectVersionTable(mock, true)
	expectUnlock(mock, 0)
	d, err := WithInstance(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec(`INSERT INTO probe`).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := d.Run(strings.NewReader(`INSERT INTO probe VALUES('a\';b');`)); err != nil {
		t.Fatal(err)
	}
	mock.ExpectClose()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWithInstanceCreatesStateUnderSessionLockInEveryMode(t *testing.T) {
	for _, mode := range []int{0, 2, 4, 7} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			db, mock := mockDB(t)
			expectMetadata(mock, mode, 1, 0)
			expectLock(mock, 0)
			expectVersionTable(mock, false)
			expectUnlock(mock, 0)
			d, err := WithInstance(db, nil)
			if err != nil {
				t.Fatal(err)
			}
			mock.ExpectClose()
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
			if err := db.Ping(); err == nil {
				t.Fatal("successful construction did not transfer pool ownership")
			}
			if err := d.Close(); err != nil {
				t.Fatalf("repeat Close: %v", err)
			}
		})
	}
}

func TestConstructionFailureDiscardsSessionButLeavesCallerPool(t *testing.T) {
	db, mock := mockDB(t)
	expectMetadata(mock, 0, 1, 0)
	expectLock(mock, 0)
	mock.ExpectQuery(`SELECT COUNT`).WillReturnError(errors.New("catalog denied"))
	expectUnlock(mock, 0)
	mock.ExpectClose()
	if _, err := WithInstance(db, nil); err == nil {
		t.Fatal("accepted metadata failure")
	}
	if err := db.Ping(); err != nil && strings.Contains(err.Error(), "database is closed") {
		t.Fatal("closed caller pool on failure")
	}
	if _, err := WithInstance(nil, nil); err == nil {
		t.Fatal("accepted nil pool")
	}
}

func TestInitializationRejectsNonnumericVersionColumns(t *testing.T) {
	db, mock := mockDB(t)
	expectMetadata(mock, 0, 1, 0)
	expectLock(mock, 0)
	mock.ExpectQuery(`SELECT COUNT`).WillReturnRows(sqlmock.NewRows([]string{countColumn}).AddRow(1))
	mock.ExpectQuery(`SELECT "VERSION", "DIRTY".*WHERE 1 = 0`).WillReturnRows(sqlmock.NewRowsWithColumnDefinition(
		sqlmock.NewColumn(versionColumn).OfType("VARCHAR", ""), sqlmock.NewColumn(dirtyColumn).OfType("SMALLINT", int64(0))))
	expectUnlock(mock, 0)
	mock.ExpectClose()
	if _, err := WithInstance(db, nil); err == nil {
		t.Fatal("accepted text history")
	}
}

func TestLockReturnCodesAndFailedUnlock(t *testing.T) {
	d, mock := initializedDriver(t)
	d.config.StatementTimeout = 10 * time.Millisecond
	for _, code := range []int{1, 2, 3, 4, 5} {
		expectLock(mock, code)
		err := d.Lock()
		if err == nil || (code == 1 && !errors.Is(err, database.ErrLocked)) || (code != 1 && errors.Is(err, database.ErrLocked)) {
			t.Errorf("lock code %d: %v", code, err)
		}
	}
	d.config.StatementTimeout = 0
	expectLock(mock, 0)
	if err := d.Lock(); err != nil {
		t.Fatal(err)
	}
	if err := d.Lock(); !errors.Is(err, database.ErrLocked) {
		t.Fatal(err)
	}
	expectUnlock(mock, 4)
	if err := d.Unlock(); err == nil {
		t.Fatal("accepted failed release")
	}
	expectUnlock(mock, 0)
	if err := d.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := d.Unlock(); !errors.Is(err, database.ErrNotLocked) {
		t.Fatal(err)
	}
}

func TestSetVersionPreservesNilDirtyAndRollsBackInsertFailure(t *testing.T) {
	d, mock := initializedDriver(t)
	for _, tc := range []struct{ version, dirty int }{{-1, 1}, {-1, 0}, {5, 0}, {5, 1}} {
		mock.ExpectBegin()
		mock.ExpectExec(`DELETE FROM "APP"\."SCHEMA_MIGRATIONS"`).WillReturnResult(sqlmock.NewResult(0, 1))
		if tc.version >= 0 || tc.dirty == 1 {
			mock.ExpectExec(`INSERT INTO .* \("VERSION", "DIRTY"\) VALUES \(\?, \?\)`).WithArgs(tc.version, tc.dirty).WillReturnResult(sqlmock.NewResult(0, 1))
		}
		mock.ExpectCommit()
		if err := d.SetVersion(tc.version, tc.dirty == 1); err != nil {
			t.Fatal(err)
		}
	}
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO`).WithArgs(2, 1).WillReturnError(errors.New("insert failed"))
	mock.ExpectRollback()
	if err := d.SetVersion(2, true); err == nil {
		t.Fatal("ignored insert failure")
	}
	if err := d.SetVersion(-2, false); err == nil {
		t.Fatal("accepted version below NilVersion")
	}
}

func TestVersionRejectsCorruptHistory(t *testing.T) {
	d, mock := initializedDriver(t)
	for _, tc := range []struct {
		rows  *sqlmock.Rows
		fail  bool
		v     int
		dirty bool
	}{
		{sqlmock.NewRows([]string{versionColumn, dirtyColumn}), false, -1, false},
		{sqlmock.NewRows([]string{versionColumn, dirtyColumn}).AddRow(-1, 1), false, -1, true},
		{sqlmock.NewRows([]string{versionColumn, dirtyColumn}).AddRow(10, 0), false, 10, false},
		{sqlmock.NewRows([]string{versionColumn, dirtyColumn}).AddRow(1, 2), true, 0, false},
		{sqlmock.NewRows([]string{versionColumn, dirtyColumn}).AddRow(-2, 1), true, 0, false},
		{sqlmock.NewRows([]string{versionColumn, dirtyColumn}).AddRow(1, 0).AddRow(2, 0), true, 0, false},
	} {
		mock.ExpectQuery(`SELECT "VERSION", "DIRTY"`).WillReturnRows(tc.rows)
		v, dirty, err := d.Version()
		if (err != nil) != tc.fail || !tc.fail && (v != tc.v || dirty != tc.dirty) {
			t.Fatalf("version: %d %v %v", v, dirty, err)
		}
	}
}

func TestRunPreflightsWholeScriptAndStopsAtFirstFailure(t *testing.T) {
	d, mock := initializedDriver(t)
	if err := d.Run(strings.NewReader("CREATE TABLE should_not_run(id INT); SELECT 'broken")); err == nil {
		t.Fatal("executed script with malformed tail")
	}
	mock.ExpectExec(`CREATE TABLE t\(id INT\)`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO t VALUES\(1\)`).WillReturnError(errors.New("DML rejected"))
	if err := d.Run(strings.NewReader("CREATE TABLE t(id INT); INSERT INTO t VALUES(1); DELETE FROM t;")); err == nil {
		t.Fatal("ignored execution error")
	}
	// A normal SQL failure permits upstream Force and recovery on the same session.
	mock.ExpectQuery(`SELECT "VERSION", "DIRTY"`).WillReturnRows(sqlmock.NewRows([]string{versionColumn, dirtyColumn}).AddRow(1, 1))
	if _, dirty, err := d.Version(); err != nil || !dirty {
		t.Fatalf("normal SQL error poisoned session: %v", err)
	}
}

func TestConnectionFailurePermanentlyDiscardsReservedSession(t *testing.T) {
	for _, failure := range []error{driver.ErrBadConn, &dmclient.DmError{ErrCode: 6001}, &dmclient.DmError{ErrCode: 20000}, context.DeadlineExceeded} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			d, mock := initializedDriver(t)
			mock.ExpectExec(`SELECT 1`).WillReturnError(failure)
			mock.ExpectClose()
			if err := d.Run(strings.NewReader("SELECT 1")); err == nil {
				t.Fatal("ignored lost session")
			}
			if _, _, err := d.Version(); err == nil {
				t.Fatal("replaced a lost migration session")
			}
		})
	}
}

func TestTimeoutDiscardsSessionAndCloseDiscardsFailedUnlock(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		d, mock := initializedDriver(t)
		d.config.StatementTimeout = time.Millisecond
		mock.ExpectExec(`SELECT 1`).WillDelayFor(100 * time.Millisecond).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectClose()
		if err := d.Run(strings.NewReader("SELECT 1")); err == nil {
			t.Fatal("timeout succeeded")
		}
		if err := d.Run(strings.NewReader("SELECT 2")); err == nil {
			t.Fatal("continued after timeout")
		}
	})
	t.Run("unlock", func(t *testing.T) {
		d, mock := initializedDriver(t)
		expectLock(mock, 0)
		if err := d.Lock(); err != nil {
			t.Fatal(err)
		}
		expectUnlock(mock, 3)
		mock.ExpectClose()
		if err := d.Close(); err == nil {
			t.Fatal("lost unlock error")
		}
	})
}

func TestCaseInsensitiveTableAliasesUseSameLockIdentity(t *testing.T) {
	var lockIDs []int64
	for _, table := range []string{"history", historyTable} {
		db, mock := mockDB(t)
		expectMetadata(mock, 4, 0, 0)
		expectLock(mock, 0)
		mock.ExpectQuery(`SELECT COUNT\(\*\).*UPPER\(NAME\) = \?`).WithArgs(101, historyTable).WillReturnRows(sqlmock.NewRows([]string{countColumn}).AddRow(1))
		mock.ExpectQuery(`SELECT "VERSION", "DIRTY".*WHERE 1 = 0`).WillReturnRows(versionColumns())
		mock.ExpectQuery(`SELECT "VERSION", "DIRTY"`).WillReturnRows(sqlmock.NewRows([]string{versionColumn, dirtyColumn}))
		expectUnlock(mock, 0)
		d, err := WithInstance(db, &Config{MigrationsTable: table})
		if err != nil {
			t.Fatal(err)
		}
		lockIDs = append(lockIDs, d.(*Dm).lockID)
		mock.ExpectClose()
		if err := d.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if lockIDs[0] != lockIDs[1] || lockIDs[0] < 0 || lockIDs[0] > 1073741823 {
		t.Fatalf("aliases have different/out-of-range locks: %v", lockIDs)
	}
}

func TestCommitAndRollbackFailurePoisonStateSession(t *testing.T) {
	for _, commitFailure := range []bool{false, true} {
		d, mock := initializedDriver(t)
		mock.ExpectBegin()
		q := mock.ExpectExec(`DELETE FROM`)
		if commitFailure {
			q.WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit().WillReturnError(errors.New("commit outcome unknown"))
		} else {
			q.WillReturnError(errors.New("delete failed"))
			mock.ExpectRollback().WillReturnError(errors.New("rollback failed"))
		}
		mock.ExpectClose()
		if err := d.SetVersion(-1, false); err == nil {
			t.Fatal("accepted uncertain transaction result")
		}
		if err := d.SetVersion(1, false); err == nil {
			t.Fatal("reused uncertain transaction session")
		}
	}
}

func TestInitializationUnlockFailureDiscardsSession(t *testing.T) {
	db, mock := mockDB(t)
	expectMetadata(mock, 7, 1, 0)
	expectLock(mock, 0)
	expectVersionTable(mock, true)
	expectUnlock(mock, 5)
	mock.ExpectClose()
	if _, err := WithInstance(db, nil); err == nil {
		t.Fatal("accepted unreleased initialization lock")
	}
}
