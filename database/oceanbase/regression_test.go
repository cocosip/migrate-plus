package oceanbase

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	ob "github.com/cocosip/go-oceanbase"
	"github.com/golang-migrate/migrate/v4/database"
)

func versionColumnRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{objectNameColumn, "type", "precision", "scale", "nullable"})
}

func TestSetVersionRollbackPreservesOriginalError(t *testing.T) {
	d, mock := mockDriver(t, ob.ModeMySQL)
	expectSession(mock, d.mode, "APP")
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM`).WillReturnResult(sqlmock.NewResult(0, 1))
	cause := errors.New("state insert failed")
	mock.ExpectExec(`INSERT INTO`).WithArgs(7, 0).WillReturnError(cause)
	mock.ExpectRollback()
	var stateErr *database.Error
	if err := d.SetVersion(7, false); !errors.As(err, &stateErr) || !errors.Is(stateErr.OrigErr, cause) {
		t.Fatalf("SetVersion=%v", err)
	}
	if d.broken.Load() {
		t.Fatal("ordinary SQL failure discarded healthy session")
	}
}

func TestUnknownCommitDiscardsPhysicalSession(t *testing.T) {
	d, mock := mockDriver(t, ob.ModeOracle)
	expectSession(mock, d.mode, "APP")
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO`).WithArgs(7, 1).WillReturnResult(sqlmock.NewResult(0, 1))
	cause := errors.New("unknown commit outcome")
	mock.ExpectCommit().WillReturnError(cause)
	var stateErr *database.Error
	if err := d.SetVersion(7, true); !errors.As(err, &stateErr) || !errors.Is(stateErr.OrigErr, cause) || !d.broken.Load() {
		t.Fatalf("commit=%v broken=%v", err, d.broken.Load())
	}
	if _, _, err := d.Version(); !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("reused lost session: %v", err)
	}
}

func TestNilVersionState(t *testing.T) {
	d, mock := mockDriver(t, ob.ModeMySQL)
	mock.ExpectQuery(`SELECT version, dirty FROM`).WillReturnRows(sqlmock.NewRows([]string{versionColumn, dirtyColumn}))
	if version, dirty, err := d.Version(); err != nil || version != database.NilVersion || dirty {
		t.Fatalf("empty=%d %v %v", version, dirty, err)
	}
	expectSession(mock, d.mode, "APP")
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := d.SetVersion(database.NilVersion, false); err != nil {
		t.Fatal(err)
	}
	expectSession(mock, d.mode, "APP")
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO`).WithArgs(-1, 1).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := d.SetVersion(database.NilVersion, true); err != nil {
		t.Fatal(err)
	}
	if err := d.SetVersion(-2, false); err == nil {
		t.Fatal("accepted invalid version")
	}
}

func TestValidateExistingVersionColumns(t *testing.T) {
	for _, mode := range []ob.Mode{ob.ModeMySQL, ob.ModeOracle} {
		d, mock := mockDriver(t, mode)
		mock.ExpectQuery(`SELECT column_name`).WillReturnRows(versionColumnRows().AddRow("VERSION", "NUMBER", 19, 0, "N").AddRow("DIRTY", "NUMBER", 1, 0, "N"))
		if err := d.validateVersionColumns(context.Background()); err != nil {
			t.Fatal(err)
		}
		mock.ExpectQuery(`SELECT version, dirty FROM`).WillReturnRows(sqlmock.NewRows([]string{versionColumn, dirtyColumn}).AddRow(7, 1))
		if version, dirty, err := d.Version(); err != nil || version != 7 || !dirty {
			t.Fatalf("existing state=%d %v %v", version, dirty, err)
		}
		mock.ExpectQuery(`SELECT column_name`).WillReturnRows(versionColumnRows().AddRow("VERSION", "NUMBER", 19, 1, "N").AddRow("DIRTY", "NUMBER", 1, 0, "N"))
		if err := d.validateVersionColumns(context.Background()); err == nil {
			t.Fatal("accepted fractional version column")
		}
	}
}

func TestValidateExistingVersionColumnsRejectsNarrowVersion(t *testing.T) {
	for _, mode := range []ob.Mode{ob.ModeMySQL, ob.ModeOracle} {
		d, mock := mockDriver(t, mode)
		rows := versionColumnRows()
		if mode == ob.ModeMySQL {
			rows.AddRow("VERSION", "TINYINT", 3, 0, "NO").AddRow("DIRTY", "TINYINT", 3, 0, "NO")
		} else {
			rows.AddRow("VERSION", "NUMBER", 10, 0, "N").AddRow("DIRTY", "NUMBER", 1, 0, "N")
		}
		mock.ExpectQuery(`SELECT column_name`).WillReturnRows(rows)
		if err := d.validateVersionColumns(context.Background()); err == nil {
			t.Fatalf("%v: accepted a migration version column without 64-bit capacity", mode)
		}
	}
}

func TestValidateExistingVersionColumnsRejectsUnsignedMySQLVersion(t *testing.T) {
	d, mock := mockDriver(t, ob.ModeMySQL)
	mock.ExpectQuery(`SELECT column_name`).WillReturnRows(versionColumnRows().
		AddRow("VERSION", "BIGINT", 20, 0, "NO").AddRow("DIRTY", "TINYINT", 3, 0, "NO"))
	if err := d.validateVersionColumns(context.Background()); err == nil {
		t.Fatal("accepted an unsigned BIGINT version column that cannot store NilVersion")
	}
	d, mock = mockDriver(t, ob.ModeMySQL)
	mock.ExpectQuery(`SELECT column_name`).WillReturnRows(versionColumnRows().
		AddRow("VERSION", "BIGINT", 19, 0, "NO").AddRow("DIRTY", "TINYINT", 3, 0, "NO"))
	if err := d.validateVersionColumns(context.Background()); err != nil {
		t.Fatalf("rejected a signed BIGINT version column: %v", err)
	}
}

func TestOperationErrorPreservesServerDetails(t *testing.T) {
	d, _ := mockDriver(t, ob.ModeMySQL)
	cause := &ob.Error{Code: 1064, SQLState: "42000", Message: "invalid SQL"}
	var native *ob.Error
	var operationErr *database.Error
	if err := d.operationError(context.Background(), "invalid", cause); !errors.As(err, &operationErr) || !errors.As(operationErr.OrigErr, &native) || native.Code != 1064 || d.broken.Load() {
		t.Fatalf("server error=%v", err)
	}
	if err := d.operationError(context.Background(), "lost", io.EOF); !errors.As(err, &operationErr) || !errors.Is(operationErr.OrigErr, io.EOF) || !d.broken.Load() {
		t.Fatalf("network error=%v", err)
	}
}

func TestOracleDropOrdersTypeDependencies(t *testing.T) {
	base := dropObject{name: "base_type", kind: "TYPE"}
	child := dropObject{name: "child_type", kind: "TYPE"}
	objects, err := orderDrop([]dropObject{base, child}, []dropDependency{{object: child, reference: base}})
	if err != nil || len(objects) != 2 || objects[0] != child {
		t.Fatalf("order=%v %v", objects, err)
	}
	if _, err := orderDrop([]dropObject{base, child}, []dropDependency{{object: child, reference: base}, {object: base, reference: child}}); err == nil {
		t.Fatal("accepted dependency cycle")
	}
}

func TestWithInstanceValidatesBeforeReservingConnection(t *testing.T) {
	if _, err := WithInstance(nil, nil); err == nil {
		t.Fatal("accepted nil instance")
	}
}
