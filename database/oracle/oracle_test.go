package oracle

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/golang-migrate/migrate/v4/database"
	go_ora "github.com/sijms/go-ora/v2"
)

const (
	countColumn      = "count"
	objectTypeColumn = "object_type"
	objectNameColumn = "object_name"
)

// Oracle output parameters are passed through database/sql, not wire values.
type outputConverter struct{}

func (outputConverter) ConvertValue(value any) (driver.Value, error) {
	switch value.(type) {
	case sql.Out, go_ora.Out:
		return value, nil
	default:
		return driver.DefaultParameterConverter.ConvertValue(value)
	}
}

type outputValue struct{ value any }

func (out outputValue) Match(value driver.Value) bool {
	var dest any
	switch arg := value.(type) {
	case sql.Out:
		dest = arg.Dest
	case go_ora.Out:
		dest = arg.Dest
	default:
		return false
	}
	reflect.ValueOf(dest).Elem().Set(reflect.ValueOf(out.value))
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

func expectLock(mock sqlmock.Sqlmock, code int) {
	mock.ExpectExec(`BEGIN :1 := DBMS_LOCK.REQUEST\(id => :2, lockmode => 6, timeout => 0, release_on_commit => FALSE\); END;`).
		WithArgs(outputValue{code}, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 0))
}

func expectUnlock(mock sqlmock.Sqlmock, code int) {
	mock.ExpectExec(`BEGIN :1 := DBMS_LOCK.RELEASE\(:2\); END;`).
		WithArgs(outputValue{code}, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 0))
}

func expectMetadata(mock sqlmock.Sqlmock, server, compatible string) {
	mock.ExpectExec(`BEGIN DBMS_UTILITY.DB_VERSION\(:1, :2\); END;`).
		WithArgs(outputValue{server}, outputValue{compatible}).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT SYS_CONTEXT\('USERENV', 'CURRENT_SCHEMA'\) FROM dual`).
		WillReturnRows(sqlmock.NewRows([]string{"schema"}).AddRow("APP"))
}

func versionRows() *sqlmock.Rows { return sqlmock.NewRows([]string{"version", "dirty"}) }

func typeDependencyRows() *sqlmock.Rows { return sqlmock.NewRows([]string{"name", "referenced"}) }

func initializedDriver(t *testing.T) (*Oracle, sqlmock.Sqlmock) {
	t.Helper()
	db, mock := mockDB(t)
	expectMetadata(mock, "19.0.0.0.0", testVersion19)
	expectLock(mock, 0)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM ALL_TABLES WHERE OWNER = :1 AND TABLE_NAME = :2`).
		WithArgs("APP", "SCHEMA_MIGRATIONS").WillReturnRows(sqlmock.NewRows([]string{countColumn}).AddRow(1))
	mock.ExpectQuery(`SELECT version, dirty FROM`).WillReturnRows(versionRows())
	expectUnlock(mock, 0)
	d, err := WithInstance(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	o := d.(*Oracle)
	t.Cleanup(func() {
		if db.Stats().OpenConnections > 0 {
			mock.ExpectClose()
		}
		if err := o.Close(); err != nil {
			t.Error(err)
		}
	})
	return o, mock
}

func TestWithInstanceCreatesVersionTableUnderSessionLock(t *testing.T) {
	db, mock := mockDB(t)
	expectMetadata(mock, "10.2.0.5.0", "10.2.0")
	expectLock(mock, 0)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM ALL_TABLES WHERE OWNER = :1 AND TABLE_NAME = :2`).
		WithArgs("APP", "HISTORY").WillReturnRows(sqlmock.NewRows([]string{countColumn}).AddRow(0))
	mock.ExpectExec(`CREATE TABLE "APP"\."HISTORY" \(version NUMBER\(19\) NOT NULL PRIMARY KEY, dirty NUMBER\(1\) NOT NULL CHECK \(dirty IN \(0, 1\)\)\)`).
		WillReturnResult(sqlmock.NewResult(0, 0))
	expectUnlock(mock, 0)
	d, err := WithInstance(db, &Config{MigrationsTable: "history"})
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectClose()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWithInstanceRejectsNilPoolAndInvalidConfig(t *testing.T) {
	if _, err := WithInstance(nil, nil); err == nil {
		t.Fatal("accepted nil pool")
	}
	db, _ := mockDB(t)
	if _, err := WithInstance(db, &Config{MigrationsTable: "bad.name"}); err == nil {
		t.Fatal("accepted invalid table")
	}
}

func TestWithInstanceReleasesLockOnInitializationFailure(t *testing.T) {
	db, mock := mockDB(t)
	expectMetadata(mock, "19.0.0.0.0", testVersion19)
	expectLock(mock, 0)
	mock.ExpectQuery(`SELECT COUNT`).WillReturnError(errors.New("metadata denied"))
	expectUnlock(mock, 0)
	mock.ExpectClose()
	if _, err := WithInstance(db, nil); err == nil {
		t.Fatal("initialization succeeded after metadata error")
	}
	// sqlmock cannot reopen discarded sessions; the pool itself must stay open.
	if err := db.Ping(); err != nil && strings.Contains(err.Error(), "database is closed") {
		t.Fatalf("caller pool was closed: %v", err)
	}
}

func TestWithInstanceDiscardsSessionWhenInitialUnlockFails(t *testing.T) {
	db, mock := mockDB(t)
	expectMetadata(mock, "19.0.0.0.0", testVersion19)
	expectLock(mock, 0)
	mock.ExpectQuery(`SELECT COUNT`).WillReturnRows(sqlmock.NewRows([]string{countColumn}).AddRow(1))
	mock.ExpectQuery(`SELECT version, dirty FROM`).WillReturnRows(versionRows())
	expectUnlock(mock, 5)
	mock.ExpectClose()
	if _, err := WithInstance(db, nil); err == nil {
		t.Fatal("initialization accepted a lock release failure")
	}
}

func TestWithInstanceRejectsLongIdentifiersUnderOldCompatibility(t *testing.T) {
	db, mock := mockDB(t)
	mock.ExpectExec(`BEGIN DBMS_UTILITY.DB_VERSION`).WithArgs(outputValue{testVersion19}, outputValue{testVersion11}).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if _, err := WithInstance(db, &Config{MigrationsTable: "M" + strings.Repeat("X", 30)}); err == nil {
		t.Fatal("accepted long identifier under old COMPATIBLE")
	}
}

func TestCloseReleasesHeldLock(t *testing.T) {
	o, mock := initializedDriver(t)
	expectLock(mock, 0)
	if err := o.Lock(); err != nil {
		t.Fatal(err)
	}
	expectUnlock(mock, 0)
	// initializedDriver's cleanup calls Close while the lock remains held.
}

func TestSetVersionHandlesTransactionFailures(t *testing.T) {
	for _, stage := range []string{"begin", "delete", "commit"} {
		t.Run(stage, func(t *testing.T) {
			o, mock := initializedDriver(t)
			failure := errors.New("transaction failed")
			if stage == "begin" {
				mock.ExpectBegin().WillReturnError(failure)
			} else {
				mock.ExpectBegin()
				if stage == "delete" {
					mock.ExpectExec(`DELETE FROM`).WillReturnError(failure)
					mock.ExpectRollback()
				} else {
					mock.ExpectExec(`DELETE FROM`).WillReturnResult(sqlmock.NewResult(0, 0))
					mock.ExpectCommit().WillReturnError(failure)
				}
			}
			if err := o.SetVersion(-1, false); err == nil {
				t.Fatal("transaction failure was swallowed")
			}
		})
	}
}

func TestLockContentionAndUnlockFailureCanBeRetried(t *testing.T) {
	o, mock := initializedDriver(t)
	o.config.StatementTimeout = 10 * time.Millisecond
	expectLock(mock, 1)
	if err := o.Lock(); !errors.Is(err, database.ErrLocked) {
		t.Fatalf("contention = %v", err)
	}
	o.config.StatementTimeout = 0
	expectLock(mock, 0)
	if err := o.Lock(); err != nil {
		t.Fatal(err)
	}
	if err := o.Lock(); !errors.Is(err, database.ErrLocked) {
		t.Fatalf("second lock = %v", err)
	}
	expectUnlock(mock, 5)
	if err := o.Unlock(); err == nil {
		t.Fatal("failed release reported success")
	}
	expectUnlock(mock, 0)
	if err := o.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := o.Unlock(); !errors.Is(err, database.ErrNotLocked) {
		t.Fatalf("second unlock = %v", err)
	}
}

func TestSetVersionPreservesDirtyNilVersionAndRollsBackFailures(t *testing.T) {
	o, mock := initializedDriver(t)
	for _, tc := range []struct {
		version int
		dirty   bool
	}{{7, true}, {7, false}, {database.NilVersion, true}, {database.NilVersion, false}} {
		mock.ExpectBegin()
		mock.ExpectExec(`DELETE FROM "APP"\."SCHEMA_MIGRATIONS"`).WillReturnResult(sqlmock.NewResult(0, 1))
		if tc.version >= 0 || tc.dirty {
			dirty := 0
			if tc.dirty {
				dirty = 1
			}
			mock.ExpectExec(`INSERT INTO "APP"\."SCHEMA_MIGRATIONS" \(version, dirty\) VALUES \(:1, :2\)`).
				WithArgs(tc.version, dirty).WillReturnResult(sqlmock.NewResult(0, 1))
		}
		mock.ExpectCommit()
		if err := o.SetVersion(tc.version, tc.dirty); err != nil {
			t.Fatal(err)
		}
	}
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO`).WillReturnError(errors.New("insert failed"))
	mock.ExpectRollback()
	if err := o.SetVersion(8, false); err == nil {
		t.Fatal("insert failure was swallowed")
	}
	mock.ExpectQuery(`SELECT version, dirty FROM`).WillReturnRows(versionRows().AddRow(7, 0))
	if version, dirty, err := o.Version(); err != nil || version != 7 || dirty {
		t.Fatalf("session unusable after successful rollback: %d, %v, %v", version, dirty, err)
	}
	if err := o.SetVersion(-2, false); err == nil {
		t.Fatal("accepted invalid version")
	}
}

func TestVersionReadsNumericDirtyAndNilState(t *testing.T) {
	o, mock := initializedDriver(t)
	for _, tc := range []struct {
		rows    *sqlmock.Rows
		version int
		dirty   bool
	}{
		{versionRows(), -1, false},
		{versionRows().AddRow(-1, 1), -1, true},
		{versionRows().AddRow(7, 0), 7, false},
	} {
		mock.ExpectQuery(`SELECT version, dirty FROM "APP"\."SCHEMA_MIGRATIONS"$`).WillReturnRows(tc.rows)
		version, dirty, err := o.Version()
		if err != nil || version != tc.version || dirty != tc.dirty {
			t.Fatalf("version = %d, %v, %v", version, dirty, err)
		}
	}
}

func TestRunStopsOnFailureAndUsesUpstreamDatabaseErrors(t *testing.T) {
	o, mock := initializedDriver(t)
	mock.ExpectExec(`CREATE TABLE t \(id NUMBER\)`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO t VALUES \(1\)`).WillReturnError(errors.New("DML failed"))
	err := o.Run(strings.NewReader("CREATE TABLE t (id NUMBER); INSERT INTO t VALUES (1); DROP TABLE t;"))
	var dbErr *database.Error
	if !errors.As(err, &dbErr) || string(dbErr.Query) != "INSERT INTO t VALUES (1)" {
		t.Fatalf("wrong execution error: %v", err)
	}
}

func TestRunRejectsMalformedScriptBeforeExecuting(t *testing.T) {
	o, _ := initializedDriver(t)
	if err := o.Run(strings.NewReader("CREATE TABLE t (id NUMBER); SELECT 'unclosed")); err == nil {
		t.Fatal("accepted malformed migration")
	}
	if err := o.Run(failingReader{}); err == nil {
		t.Fatal("ignored migration read failure")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, fmt.Errorf("read failed") }

func TestRunBoundsStatementExecution(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o, mock := initializedDriver(t)
		o.config.StatementTimeout = time.Millisecond
		mock.ExpectExec(`BEGIN NULL; END;`).WillDelayFor(100 * time.Millisecond).WillReturnResult(sqlmock.NewResult(0, 0))
		var dbErr *database.Error
		if err := o.Run(strings.NewReader("BEGIN NULL; END;\n/")); !errors.As(err, &dbErr) || !errors.Is(dbErr.OrigErr, context.DeadlineExceeded) {
			t.Fatalf("statement deadline error was lost: %v", err)
		}
		if err := o.Run(strings.NewReader("SELECT 1 FROM dual;")); !errors.Is(err, sql.ErrConnDone) {
			t.Fatalf("reused a timed out session: %v", err)
		}
	})
}

func expectDropPreflight(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM`).WithArgs("APP", "APP", "APP", "APP").
		WillReturnRows(sqlmock.NewRows([]string{countColumn}).AddRow(0))
	mock.ExpectQuery(`SELECT t.object_name, d.referenced_name FROM ALL_OBJECTS`).WithArgs("APP").
		WillReturnRows(typeDependencyRows())
	mock.ExpectQuery(`SELECT table_name, constraint_name FROM ALL_CONSTRAINTS`).WithArgs("APP").
		WillReturnRows(sqlmock.NewRows([]string{"table", "constraint"}))
}

func TestDropQuotesSchemaObjectsWithoutCascading(t *testing.T) {
	o, mock := initializedDriver(t)
	expectDropPreflight(mock)
	mock.ExpectQuery(`SELECT object_type, object_name FROM ALL_OBJECTS`).WithArgs("APP", "APP").
		WillReturnRows(sqlmock.NewRows([]string{objectTypeColumn, objectNameColumn}))
	mock.ExpectQuery(`SELECT object_type, object_name FROM ALL_OBJECTS`).WithArgs("APP", "APP").
		WillReturnRows(sqlmock.NewRows([]string{objectTypeColumn, objectNameColumn}).
			AddRow("VIEW", "v\"x").AddRow("TABLE", "T").AddRow("SEQUENCE", "S"))
	mock.ExpectExec(`DROP VIEW "APP"\."v""x"`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DROP TABLE "APP"\."T" PURGE$`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DROP SEQUENCE "APP"\."S"`).WillReturnResult(sqlmock.NewResult(0, 0))
	if err := o.Drop(); err != nil {
		t.Fatal(err)
	}
}

func TestDropMaterializedViewsBeforeListingRemainingTables(t *testing.T) {
	for _, prebuilt := range []bool{false, true} {
		t.Run(fmt.Sprintf("prebuilt=%v", prebuilt), func(t *testing.T) {
			o, mock := initializedDriver(t)
			expectDropPreflight(mock)
			mock.ExpectQuery(`SELECT object_type, object_name FROM ALL_OBJECTS[\s\S]*object_type IN \('MATERIALIZED VIEW'\)`).
				WithArgs("APP", "APP").
				WillReturnRows(sqlmock.NewRows([]string{objectTypeColumn, objectNameColumn}).AddRow("MATERIALIZED VIEW", "CACHE"))
			mock.ExpectExec(`DROP MATERIALIZED VIEW "APP"\."CACHE"`).WillReturnResult(sqlmock.NewResult(0, 0))
			// Ordinary containers disappear with the view; prebuilt tables remain.
			remaining := sqlmock.NewRows([]string{objectTypeColumn, objectNameColumn})
			if prebuilt {
				remaining.AddRow("TABLE", "CACHE")
			}
			mock.ExpectQuery(`SELECT object_type, object_name FROM ALL_OBJECTS[\s\S]*object_type IN \('VIEW', 'TABLE',`).
				WithArgs("APP", "APP").WillReturnRows(remaining)
			if prebuilt {
				mock.ExpectExec(`DROP TABLE "APP"\."CACHE" PURGE$`).WillReturnResult(sqlmock.NewResult(0, 0))
			}
			if err := o.Drop(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDropStopsOnMaterializedViewPhaseErrors(t *testing.T) {
	for _, stage := range []string{"list views", "drop view", "list remaining objects"} {
		t.Run(stage, func(t *testing.T) {
			o, mock := initializedDriver(t)
			expectDropPreflight(mock)
			failure := errors.New("drop failed")
			query := mock.ExpectQuery(`SELECT object_type, object_name FROM ALL_OBJECTS`).WithArgs("APP", "APP")
			if stage == "list views" {
				query.WillReturnError(failure)
			} else {
				query.WillReturnRows(sqlmock.NewRows([]string{objectTypeColumn, objectNameColumn}).AddRow("MATERIALIZED VIEW", "CACHE"))
				drop := mock.ExpectExec(`DROP MATERIALIZED VIEW "APP"\."CACHE"`)
				if stage == "drop view" {
					drop.WillReturnError(failure)
				} else {
					drop.WillReturnResult(sqlmock.NewResult(0, 0))
					mock.ExpectQuery(`SELECT object_type, object_name FROM ALL_OBJECTS`).WithArgs("APP", "APP").WillReturnError(failure)
				}
			}
			var dbErr *database.Error
			if err := o.Drop(); !errors.As(err, &dbErr) || !errors.Is(dbErr.OrigErr, failure) {
				t.Fatalf("drop error = %v, want %v", err, failure)
			}
		})
	}
}

func TestDriverRegistersWithUpstream(t *testing.T) {
	found := false
	for _, name := range database.List() {
		found = found || name == "oracle"
	}
	if !found {
		t.Fatal("oracle is missing from the upstream driver registry")
	}
	if _, err := database.Open("oracle://host/service?x-invalid=1"); err == nil || !strings.Contains(err.Error(), "unknown migration option") {
		t.Fatalf("upstream registry did not reach Oracle URL validation: %v", err)
	}
}
