package oceanbase

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	ob "github.com/cocosip/go-oceanbase"
	"github.com/golang-migrate/migrate/v4/database"
)

const countColumn = "count"
const versionColumn = "version"
const objectNameColumn = "name"
const objectKindColumn = "kind"
const dirtyColumn = "dirty"

type valueConverter struct{}

func (valueConverter) ConvertValue(v any) (driver.Value, error) { return v, nil }

type outputValue int

func (v outputValue) Match(value driver.Value) bool {
	out, ok := value.(sql.Out)
	if !ok {
		return false
	}
	reflect.ValueOf(out.Dest).Elem().SetInt(int64(v))
	return true
}

func mockDriver(t *testing.T, mode ob.Mode) (*OceanBase, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.ValueConverterOption(valueConverter{}))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lockCtx, cancel := context.WithCancel(context.Background())
	d := &OceanBase{db: db, conn: conn, mode: mode, schema: "APP", config: Config{MigrationsTable: "history"}, lockName: "test", lockID: 42, lockCtx: lockCtx, stopLock: cancel}
	t.Cleanup(func() {
		if d.isLocked.Load() && !d.broken.Load() {
			expectUnlock(mock, mode, successCode(mode))
		}
		if db.Stats().OpenConnections > 0 {
			mock.ExpectClose()
		}
		if err := d.Close(); err != nil {
			t.Error(err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	})
	return d, mock
}

func successCode(mode ob.Mode) int {
	if mode == ob.ModeMySQL {
		return 1
	}
	return 0
}
func expectLock(mock sqlmock.Sqlmock, mode ob.Mode, code int) {
	if mode == ob.ModeMySQL {
		mock.ExpectQuery(`SELECT GET_LOCK`).WithArgs(sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"lock"}).AddRow(code))
		return
	}
	mock.ExpectExec(`BEGIN :1 := DBMS_LOCK.REQUEST`).WithArgs(outputValue(code), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 0))
}
func expectUnlock(mock sqlmock.Sqlmock, mode ob.Mode, code int) {
	if mode == ob.ModeMySQL {
		mock.ExpectQuery(`SELECT RELEASE_LOCK`).WithArgs(sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"lock"}).AddRow(code))
		return
	}
	mock.ExpectExec(`BEGIN :1 := DBMS_LOCK.RELEASE`).WithArgs(outputValue(code), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 0))
}
func expectSession(mock sqlmock.Sqlmock, mode ob.Mode, schema string) {
	if mode == ob.ModeMySQL {
		mock.ExpectQuery(`SELECT DATABASE`).WillReturnRows(sqlmock.NewRows([]string{"schema", "autocommit", "sql_mode", "lower_case_table_names"}).AddRow(schema, 1, "", 0))
		return
	}
	mock.ExpectQuery(`SELECT SYS_CONTEXT`).WillReturnRows(sqlmock.NewRows([]string{"schema", "autocommit"}).AddRow(schema, 1))
}

func TestLocksAndVersionState(t *testing.T) {
	for _, mode := range []ob.Mode{ob.ModeMySQL, ob.ModeOracle} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			d, mock := mockDriver(t, mode)
			expectLock(mock, mode, successCode(mode))
			if err := d.Lock(); err != nil {
				t.Fatal(err)
			}
			if err := d.Lock(); !errors.Is(err, database.ErrLocked) {
				t.Fatalf("second Lock=%v", err)
			}
			expectSession(mock, mode, "APP")
			mock.ExpectBegin()
			mock.ExpectExec(`DELETE FROM`).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(`INSERT INTO`).WithArgs(7, 1).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()
			if err := d.SetVersion(7, true); err != nil {
				t.Fatal(err)
			}
			mock.ExpectQuery(`SELECT version, dirty FROM`).WillReturnRows(sqlmock.NewRows([]string{versionColumn, dirtyColumn}).AddRow(7, 1))
			if v, dirty, err := d.Version(); err != nil || v != 7 || !dirty {
				t.Fatalf("state=%d %v %v", v, dirty, err)
			}
			expectUnlock(mock, mode, successCode(mode))
			if err := d.Unlock(); err != nil {
				t.Fatal(err)
			}
			if err := d.Unlock(); !errors.Is(err, database.ErrNotLocked) {
				t.Fatalf("second Unlock=%v", err)
			}
		})
	}
}

func TestVersionRejectsCorruptHistory(t *testing.T) {
	for _, rows := range []*sqlmock.Rows{
		sqlmock.NewRows([]string{versionColumn, dirtyColumn}).AddRow(-2, 0),
		sqlmock.NewRows([]string{versionColumn, dirtyColumn}).AddRow(1, 2),
		sqlmock.NewRows([]string{versionColumn, dirtyColumn}).AddRow(1, 0).AddRow(2, 0),
	} {
		d, mock := mockDriver(t, ob.ModeMySQL)
		mock.ExpectQuery(`SELECT version, dirty FROM`).WillReturnRows(rows)
		if _, _, err := d.Version(); err == nil {
			t.Fatal("accepted corrupt history")
		}
	}
}

func TestRunPreflightsBeforeAnySQL(t *testing.T) {
	d, mock := mockDriver(t, ob.ModeMySQL)
	expectSession(mock, d.mode, "APP")
	if err := d.Run(strings.NewReader("CREATE TABLE t(id INT); USE other;")); err == nil {
		t.Fatal("accepted schema change")
	}
	expectSession(mock, d.mode, "OTHER")
	if err := d.SetVersion(1, false); err == nil {
		t.Fatal("wrote state after schema change")
	}
	if err := d.Run(strings.NewReader("SELECT 1")); err == nil {
		t.Fatal("reused discarded session")
	}
}

func TestLockTimeoutAndUnknownOutcome(t *testing.T) {
	for _, mode := range []ob.Mode{ob.ModeMySQL, ob.ModeOracle} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			d, mock := mockDriver(t, mode)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := d.lock(ctx); err == nil {
				t.Fatal("ignored cancellation")
			}
			if d.isLocked.Load() {
				t.Fatal("retained local lock flag")
			}
			_ = mock
		})
	}
}

func TestInitializationChecksLockBeforeCreatingHistory(t *testing.T) {
	for _, mode := range []ob.Mode{ob.ModeMySQL, ob.ModeOracle} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			db, mock, err := sqlmock.New(sqlmock.ValueConverterOption(valueConverter{}))
			if err != nil {
				t.Fatal(err)
			}
			modeName := "MYSQL"
			if mode == ob.ModeOracle {
				modeName = "ORACLE"
			}
			mock.ExpectQuery(`^$`).WithArgs(sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{versionColumn, "mode", "parameters", "types"}).AddRow("OceanBase 4.4.2.2", modeName, 1, 1))
			expectSession(mock, mode, "APP")
			mock.ExpectBegin()
			mock.ExpectRollback()
			expectLock(mock, mode, successCode(mode))
			for _, query := range []string{"COMMIT", "ROLLBACK"} {
				mock.ExpectExec("^" + query + "$").WillReturnResult(sqlmock.NewResult(0, 0))
				if mode == ob.ModeMySQL {
					mock.ExpectQuery(`SELECT IS_USED_LOCK`).WithArgs(sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"held"}).AddRow(1))
				} else {
					expectLock(mock, mode, 4)
				}
			}
			query := `SELECT COUNT.*create_options`
			if mode == ob.ModeOracle {
				query = `SELECT COUNT.*TEMPORARY = 'N'`
			}
			mock.ExpectQuery(query).WillReturnRows(sqlmock.NewRows([]string{countColumn}).AddRow(0))
			mock.ExpectExec(`CREATE TABLE`).WillReturnResult(sqlmock.NewResult(0, 0))
			expectUnlock(mock, mode, successCode(mode))
			d, err := WithInstance(db, nil)
			if err != nil {
				t.Fatal(err)
			}
			mock.ExpectClose()
			if err := d.Close(); err != nil {
				t.Fatal(err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLockMustSurviveCommit(t *testing.T) {
	for _, mode := range []ob.Mode{ob.ModeMySQL, ob.ModeOracle} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			d, mock := mockDriver(t, mode)
			expectLock(mock, mode, successCode(mode))
			if err := d.Lock(); err != nil {
				t.Fatal(err)
			}
			mock.ExpectExec(`^COMMIT$`).WillReturnResult(sqlmock.NewResult(0, 0))
			if mode == ob.ModeMySQL {
				mock.ExpectQuery(`SELECT IS_USED_LOCK`).WithArgs(sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"held"}).AddRow(nil))
			} else {
				expectLock(mock, mode, 0)
			}
			if err := d.verifyLockSurvival(); err == nil || !d.broken.Load() {
				t.Fatalf("unsafe lock accepted: %v", err)
			}
		})
	}
}

func TestRunBoundsEntireFile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, mock := mockDriver(t, ob.ModeMySQL)
		d.config.StatementTimeout = time.Second
		expectSession(mock, d.mode, "APP")
		mock.ExpectExec(regexp.QuoteMeta("SELECT 1;")).WillDelayFor(2 * time.Second).WillReturnResult(sqlmock.NewResult(0, 0))
		if err := d.Run(strings.NewReader("SELECT 1;")); err == nil || !d.broken.Load() {
			t.Fatalf("timeout=%v", err)
		}
	})
}
