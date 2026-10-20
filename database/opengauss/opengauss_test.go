package opengauss

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	opq "gitcode.com/opengauss/openGauss-connector-go-pq"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/golang-migrate/migrate/v4/database"
)

const testSchema = "app"

const nativeErrorSeverity = "ERROR"

const committedScript = "BEGIN; SELECT 1; COMMIT;"

func mockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
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

func expectLock(mock sqlmock.Sqlmock, acquired bool) {
	mock.ExpectQuery(`SELECT pg_catalog.pg_try_advisory_lock`).WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(acquired))
}

func expectUnlock(mock sqlmock.Sqlmock, released bool) {
	mock.ExpectQuery(`SELECT pg_catalog.pg_advisory_unlock`).WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"unlocked"}).AddRow(released))
}

func metadataRows(product, mode, schema string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"product", "database", "schema", "mode", "identifier_length", "standard_strings", "read_only", "client_encoding"}).
		AddRow(product, "appdb", schema, mode, 63, "on", settingOff, "UTF8")
}

func expectMetadata(mock sqlmock.Sqlmock, mode string, coordinators int) {
	mock.ExpectQuery(`SELECT pg_catalog.version\(\), pg_catalog.current_database\(\), pg_catalog.current_schema\(\)`).
		WillReturnRows(metadataRows("openGauss 5.0.0 (build 5af1295d) compiled at 2024-01-05", mode, testSchema))
	mock.ExpectQuery(`SELECT oid::bigint FROM pg_catalog.pg_namespace`).WithArgs(testSchema).
		WillReturnRows(sqlmock.NewRows([]string{"oid"}).AddRow(42))
	mock.ExpectQuery(`SELECT count\(\*\) FROM pg_catalog.pg_class`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT count\(\*\) FROM pg_catalog.pgxc_node`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(coordinators))
}

func versionRows() *sqlmock.Rows { return sqlmock.NewRows([]string{"version", "dirty"}) }

func expectScriptSettings(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(regexp.QuoteMeta(scriptSettingsSQL)).
		WillReturnRows(scriptSettingsRows("A", "on", "UTF8"))
}

func scriptSettingsRows(mode, standard, encoding string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"mode", "standard", "encoding"}).AddRow(mode, standard, encoding)
}

func expectRun(mock sqlmock.Sqlmock, pattern string) *sqlmock.ExpectedExec {
	expectScriptSettings(mock)
	return mock.ExpectExec(pattern)
}

func expectRollback(mock sqlmock.Sqlmock) {
	mock.ExpectExec(`ROLLBACK`).WillReturnResult(sqlmock.NewResult(0, 0))
}

func expectIdleProbe(mock sqlmock.Sqlmock) {
	mock.ExpectExec(regexp.QuoteMeta(transactionProbeSQL)).
		WillReturnError(&opq.Error{Severity: nativeErrorSeverity, Code: "25P01", Message: "SAVEPOINT can only be used in transaction blocks"})
}

func stateColumns() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"name", "type", "not_null", "primary"}).
		AddRow("version", 20, true, true).AddRow("dirty", 21, true, false)
}

func expectVersionTable(mock sqlmock.Sqlmock, exists bool) {
	expectLock(mock, true)
	relation := sqlmock.NewRows([]string{"relkind", "relpersistence"})
	if exists {
		relation.AddRow("r", "p")
	}
	mock.ExpectQuery(`SELECT c.relkind, c.relpersistence FROM pg_catalog.pg_class`).WithArgs(int64(42), "schema_migrations").WillReturnRows(relation)
	if !exists {
		mock.ExpectExec(`CREATE TABLE "app"\."schema_migrations"`).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	mock.ExpectQuery(`SELECT a.attname, a.atttypid::bigint`).WithArgs(int64(42), "schema_migrations").WillReturnRows(stateColumns())
	mock.ExpectQuery(`SELECT "version", "dirty" FROM "app"\."schema_migrations"`).WillReturnRows(versionRows())
	expectUnlock(mock, true)
}

func initializedDriver(t *testing.T) (*OpenGauss, sqlmock.Sqlmock) {
	t.Helper()
	db, mock := mockDB(t)
	expectMetadata(mock, "A", 0)
	expectVersionTable(mock, true)
	d, err := WithInstance(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	g := d.(*OpenGauss)
	t.Cleanup(func() {
		if db.Stats().OpenConnections > 0 {
			mock.ExpectClose()
		}
		if err := g.Close(); err != nil {
			t.Error(err)
		}
	})
	return g, mock
}

func TestInitializationCreatesStateUnderLock(t *testing.T) {
	db, mock := mockDB(t)
	expectMetadata(mock, "PG", 0)
	expectVersionTable(mock, false)
	d, err := WithInstance(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectClose()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInitializationRejectsDistributedSessions(t *testing.T) {
	db, mock := mockDB(t)
	expectMetadata(mock, "A", 2)
	mock.ExpectClose()
	if _, err := WithInstance(db, nil); err == nil || !strings.Contains(err.Error(), "distributed") {
		t.Fatalf("distributed initialization = %v", err)
	}
}

func TestInitializationRejectsUnknownProductModeAndSystemSchema(t *testing.T) {
	const rejectedProduct = "openGauss 5.0.0"
	for _, tc := range []struct{ product, mode, schema, want string }{
		{"PostgreSQL 16", "PG", testSchema, "PostgreSQL servers need the upstream postgres driver"},
		{"GaussDB Kernel V500R002C00", "PG", testSchema, "database/gaussdb driver"},
		{rejectedProduct, "B", testSchema, "unsupported sql_compatibility"},
		{rejectedProduct, "A", "pg_catalog", "system schema"},
		{rejectedProduct, "A", "", "cannot identify the connected database and schema"},
	} {
		t.Run(tc.product+tc.mode+tc.schema, func(t *testing.T) {
			db, mock := mockDB(t)
			mock.ExpectQuery(`SELECT pg_catalog.version`).WillReturnRows(metadataRows(tc.product, tc.mode, tc.schema))
			mock.ExpectClose()
			if _, err := WithInstance(db, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unsafe initialization = %v", err)
			}
		})
	}
}

func TestInitializationFailureKeepsCallerPool(t *testing.T) {
	db, mock := mockDB(t)
	expectMetadata(mock, "A", 0)
	expectLock(mock, true)
	mock.ExpectQuery(`SELECT c.relkind`).WillReturnError(errors.New("catalog denied"))
	expectUnlock(mock, true)
	mock.ExpectClose()
	if _, err := WithInstance(db, nil); err == nil {
		t.Fatal("ignored catalog failure")
	}
	if err := db.Ping(); err != nil && strings.Contains(err.Error(), "database is closed") {
		t.Fatalf("caller pool closed: %v", err)
	}
}

func TestWithInstanceRejectsNilPoolAndInvalidConfig(t *testing.T) {
	if _, err := WithInstance(nil, nil); err == nil {
		t.Fatal("accepted nil pool")
	}
	db, _ := mockDB(t)
	if _, err := WithInstance(db, &Config{MigrationsTable: "bad.name"}); err == nil {
		t.Fatal("accepted invalid configuration")
	}
}

func TestLockRetriesAndFailedReleasePreservesState(t *testing.T) {
	g, mock := initializedDriver(t)
	expectLock(mock, false)
	expectLock(mock, true)
	if err := g.Lock(); err != nil {
		t.Fatal(err)
	}
	if err := g.Lock(); !errors.Is(err, database.ErrLocked) {
		t.Fatalf("repeated lock = %v", err)
	}
	expectUnlock(mock, false)
	if err := g.Unlock(); err == nil || !g.isLocked.Load() {
		t.Fatalf("failed unlock lost state: %v", err)
	}
	expectUnlock(mock, true)
	if err := g.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := g.Unlock(); !errors.Is(err, database.ErrNotLocked) {
		t.Fatalf("repeated unlock = %v", err)
	}
}

func TestVersionStatePreservesNilDirtyAndRejectsCorruption(t *testing.T) {
	g, mock := initializedDriver(t)
	for _, tc := range []struct {
		version int
		dirty   bool
	}{{7, true}, {7, false}, {-1, true}, {-1, false}} {
		mock.ExpectBegin()
		mock.ExpectExec(`DELETE FROM`).WillReturnResult(sqlmock.NewResult(0, 1))
		if tc.version >= 0 || tc.dirty {
			value := 0
			if tc.dirty {
				value = 1
			}
			mock.ExpectExec(`INSERT INTO`).WithArgs(tc.version, value).WillReturnResult(sqlmock.NewResult(0, 1))
		}
		mock.ExpectCommit()
		if err := g.SetVersion(tc.version, tc.dirty); err != nil {
			t.Fatal(err)
		}
	}
	for _, rows := range []*sqlmock.Rows{
		versionRows().AddRow(1, 2),
		versionRows().AddRow(-2, 0),
		versionRows().AddRow(1, 0).AddRow(2, 0),
	} {
		mock.ExpectQuery(`SELECT "version", "dirty"`).WillReturnRows(rows)
		if _, _, err := g.Version(); err == nil {
			t.Fatal("accepted corrupt migration history")
		}
	}
	mock.ExpectQuery(`SELECT "version", "dirty"`).WillReturnRows(versionRows())
	if version, dirty, err := g.Version(); err != nil || version != -1 || dirty {
		t.Fatalf("empty version = %d %v %v", version, dirty, err)
	}
	if err := g.SetVersion(-2, false); err == nil {
		t.Fatal("accepted invalid version")
	}
}

func TestVersionTransactionRollbackAndUncertainCommit(t *testing.T) {
	g, mock := initializedDriver(t)
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO`).WillReturnError(errors.New("insert failed"))
	mock.ExpectRollback()
	if err := g.SetVersion(1, true); err == nil || g.broken.Load() {
		t.Fatalf("rollback = %v, broken=%v", err, g.broken.Load())
	}
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit().WillReturnError(errors.New("commit uncertain"))
	mock.ExpectClose()
	if err := g.SetVersion(-1, false); err == nil || !g.broken.Load() {
		t.Fatalf("uncertain commit = %v", err)
	}
	if err := g.Run(strings.NewReader("SELECT 1;")); !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("reused uncertain session: %v", err)
	}
}

func TestRunUsesWholeScriptAndClearsLeftoverTransactions(t *testing.T) {
	g, mock := initializedDriver(t)
	script := "CREATE TABLE t(id int); INSERT INTO t VALUES (1);"
	expectRun(mock, regexp.QuoteMeta(script)).WillReturnResult(sqlmock.NewResult(0, 1))
	expectIdleProbe(mock)
	if err := g.Run(strings.NewReader(script)); err != nil {
		t.Fatal(err)
	}
	// Uncommitted work must fail the migration even when cleanup succeeds.
	expectRun(mock, `BEGIN; SELECT 1;`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(transactionProbeSQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	expectRollback(mock)
	if err := g.Run(strings.NewReader("BEGIN; SELECT 1;")); err == nil || g.broken.Load() {
		t.Fatalf("unfinished transaction = %v, broken=%v", err, g.broken.Load())
	}
	// A failed script also triggers cleanup and surfaces the execution error without
	// discarding the session.
	expectRun(mock, `SELECT 1; SELECT 2`).WillReturnError(errors.New("syntax error"))
	expectRollback(mock)
	if err := g.Run(strings.NewReader("SELECT 1; SELECT 2")); err == nil || g.broken.Load() {
		t.Fatalf("failed script = %v, broken=%v", err, g.broken.Load())
	}
	expectScriptSettings(mock)
	if err := g.Run(strings.NewReader("SELECT 1; COPY t FROM STDIN;")); err == nil {
		t.Fatal("accepted COPY protocol")
	}
	if err := g.Run(strings.NewReader("   ")); err != nil {
		t.Fatalf("empty script = %v", err)
	}
	if err := g.Run(io.MultiReader(strings.NewReader("SELECT 1;"), failingReader{})); err == nil {
		t.Fatal("ignored reader failure")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("reader failed") }

type foreignStateError struct{ code string }

func (e foreignStateError) Error() string    { return "foreign wire error" }
func (e foreignStateError) SQLState() string { return e.code }

func TestForeignDriverErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		code  string
		fatal bool
	}{{"08006", true}, {"57P01", false}} {
		t.Run(tc.code, func(t *testing.T) {
			g, mock := initializedDriver(t)
			expectRun(mock, `SELECT 1`).WillReturnError(foreignStateError{code: tc.code})
			if tc.fatal {
				mock.ExpectClose()
			} else {
				expectRollback(mock)
			}
			err := g.Run(strings.NewReader("SELECT 1"))
			if err == nil {
				t.Fatal("foreign error ignored")
			}
			if g.broken.Load() != tc.fatal {
				t.Fatalf("code %s broken=%v", tc.code, g.broken.Load())
			}
		})
	}
}

func TestLostSessionAndTimeoutCannotContinue(t *testing.T) {
	for _, failure := range []error{driver.ErrBadConn, context.DeadlineExceeded} {
		t.Run(failure.Error(), func(t *testing.T) {
			g, mock := initializedDriver(t)
			expectRun(mock, `SELECT 1`).WillReturnError(failure)
			mock.ExpectClose()
			if err := g.Run(strings.NewReader("SELECT 1")); err == nil || !g.broken.Load() {
				t.Fatalf("lost session = %v", err)
			}
			if _, _, err := g.Version(); !errors.Is(err, sql.ErrConnDone) {
				t.Fatalf("reconnected after lock loss: %v", err)
			}
		})
	}
}

func TestRunTimeoutBoundsWholeFile(t *testing.T) {
	g, mock := initializedDriver(t)
	g.config.StatementTimeout = time.Millisecond
	expectRun(mock, `SELECT 1; SELECT 2;`).WillDelayFor(time.Second).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectClose()
	if err := g.Run(strings.NewReader("SELECT 1; SELECT 2;")); err == nil || !g.broken.Load() {
		t.Fatalf("ignored whole-file timeout: %v", err)
	}
}

func TestRunDiscardsSessionIfRollbackDoesNotSucceed(t *testing.T) {
	g, mock := initializedDriver(t)
	expectRun(mock, `BEGIN`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(transactionProbeSQL)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`ROLLBACK`).WillReturnError(errors.New("rollback denied"))
	mock.ExpectClose()
	if err := g.Run(strings.NewReader("BEGIN")); err == nil || !g.broken.Load() {
		t.Fatalf("unrestored transaction = %v, broken=%v", err, g.broken.Load())
	}
}

func TestVersionEOFDiscardsSession(t *testing.T) {
	for _, failure := range []error{io.EOF, io.ErrUnexpectedEOF} {
		t.Run(failure.Error(), func(t *testing.T) {
			g, mock := initializedDriver(t)
			mock.ExpectQuery(`SELECT "version", "dirty"`).WillReturnError(failure)
			mock.ExpectClose()
			_, _, err := g.Version()
			var dbErr *database.Error
			if !errors.As(err, &dbErr) || !errors.Is(dbErr.OrigErr, failure) || !g.broken.Load() {
				t.Fatalf("EOF = %v, broken=%v", err, g.broken.Load())
			}
		})
	}
}

func TestNativeServerErrorClassification(t *testing.T) {
	for _, native := range []*opq.Error{
		{Severity: "FATAL", Code: "57P01", Message: "admin shutdown"},
		{Severity: "PANIC", Code: "XX000", Message: "panic"},
		{Severity: nativeErrorSeverity, Code: "08006", Message: "connection failure"},
	} {
		t.Run(string(native.Code), func(t *testing.T) {
			g, mock := initializedDriver(t)
			expectRun(mock, `SELECT 1`).WillReturnError(native)
			mock.ExpectClose()
			err := g.Run(strings.NewReader("SELECT 1"))
			var dbErr *database.Error
			var got *opq.Error
			if err == nil || !g.broken.Load() || !errors.As(err, &dbErr) || !errors.As(dbErr.OrigErr, &got) {
				t.Fatalf("fatal native error = %v, broken=%v", err, g.broken.Load())
			}
		})
	}
	g, mock := initializedDriver(t)
	expectRun(mock, `SELECT 1`).WillReturnError(&opq.Error{Severity: nativeErrorSeverity, Code: "42601", Message: "syntax error"})
	expectRollback(mock)
	if err := g.Run(strings.NewReader("SELECT 1")); err == nil || g.broken.Load() {
		t.Fatalf("non-fatal native error = %v, broken=%v", err, g.broken.Load())
	}
}

func TestRunRefreshesEffectiveStringSettingsBeforeCOPYGuard(t *testing.T) {
	g, mock := initializedDriver(t)
	g.caps.standardStrings = false
	mock.ExpectQuery(`SELECT pg_catalog.current_setting\('sql_compatibility'\)`).
		WillReturnRows(scriptSettingsRows("PG", "on", "UTF8"))
	if err := g.Run(strings.NewReader(`SELECT '\'; COPY t FROM STDIN; --';`)); err == nil || !strings.HasPrefix(err.Error(), "opengauss:") {
		t.Fatalf("COPY after settings change was not rejected: %v", err)
	}
}

func TestRunRejectsNonUTF8ClientEncoding(t *testing.T) {
	g, mock := initializedDriver(t)
	mock.ExpectQuery(`SELECT pg_catalog.current_setting\('sql_compatibility'\)`).
		WillReturnRows(scriptSettingsRows("PG", settingOff, "GBK"))
	if err := g.Run(strings.NewReader("SELECT 1")); err == nil || !strings.Contains(err.Error(), "UTF8") {
		t.Fatalf("non-UTF8 session = %v", err)
	}
}
