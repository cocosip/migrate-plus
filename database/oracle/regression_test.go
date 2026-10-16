package oracle

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/golang-migrate/migrate/v4"
)

func TestVersionRejectsCorruptHistory(t *testing.T) {
	for _, rows := range []*sqlmock.Rows{
		versionRows().AddRow(7, 0).AddRow(8, 1),
		versionRows().AddRow(-2, 0),
		versionRows().AddRow(7, 2),
	} {
		o, mock := initializedDriver(t)
		mock.ExpectQuery(`SELECT version, dirty FROM`).WillReturnRows(rows).RowsWillBeClosed()
		if _, _, err := o.Version(); err == nil {
			t.Fatal("accepted corrupt or ambiguous migration history")
		}
	}
}

func TestDropRejectsExternalDependenciesBeforeDDL(t *testing.T) {
	o, mock := initializedDriver(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM`).WithArgs("APP", "APP", "APP", "APP").
		WillReturnRows(sqlmock.NewRows([]string{countColumn}).AddRow(1))
	if err := o.Drop(); err == nil || !strings.Contains(err.Error(), "other schemas") {
		t.Fatalf("external dependency was not rejected: %v", err)
	}
}

func TestInitializationLockUsesUpstreamTimeout(t *testing.T) {
	previous := migrate.DefaultLockTimeout
	migrate.DefaultLockTimeout = 5 * time.Millisecond
	t.Cleanup(func() { migrate.DefaultLockTimeout = previous })
	db, mock := mockDB(t)
	expectMetadata(mock, "19.0.0.0.0", testVersion19)
	expectLock(mock, 1)
	mock.ExpectClose()
	if _, err := WithInstance(db, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("initialization did not honor lock timeout: %v", err)
	}
}

func TestInitializationRejectsCorruptHistoryAndReleasesLock(t *testing.T) {
	db, mock := mockDB(t)
	expectMetadata(mock, "19.0.0.0.0", testVersion19)
	expectLock(mock, 0)
	mock.ExpectQuery(`SELECT COUNT`).WillReturnRows(sqlmock.NewRows([]string{countColumn}).AddRow(1))
	mock.ExpectQuery(`SELECT version, dirty FROM`).
		WillReturnRows(versionRows().AddRow(7, 0).AddRow(8, 1)).RowsWillBeClosed()
	expectUnlock(mock, 0)
	mock.ExpectClose()
	if _, err := WithInstance(db, nil); err == nil || !strings.Contains(err.Error(), "multiple migration state rows") {
		t.Fatalf("corrupt history accepted during initialization: %v", err)
	}
}

func TestVersionPreservesReadAndCloseErrors(t *testing.T) {
	failure := errors.New("state read failed")
	for _, stage := range []string{"query", "scan", "first row", "next row", "close"} {
		t.Run(stage, func(t *testing.T) {
			o, mock := initializedDriver(t)
			query := mock.ExpectQuery(`SELECT version, dirty FROM`)
			rows := versionRows().AddRow(7, 0)
			switch stage {
			case "query":
				query.WillReturnError(failure)
			case "scan":
				query.WillReturnRows(versionRows().AddRow(nil, 0))
			case "first row":
				query.WillReturnRows(rows.RowError(0, failure))
			case "next row":
				query.WillReturnRows(rows.AddRow(8, 1).RowError(1, failure))
			case "close":
				query.WillReturnRows(rows.CloseError(failure))
			}
			if _, _, err := o.Version(); err == nil {
				t.Fatalf("ignored %s error", stage)
			}
		})
	}
}

func TestDropOrdersTypesAndRemovesOnlyLocalForeignKeys(t *testing.T) {
	o, mock := initializedDriver(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM`).WithArgs("APP", "APP", "APP", "APP").
		WillReturnRows(sqlmock.NewRows([]string{countColumn}).AddRow(0))
	mock.ExpectQuery(`SELECT t.object_name, d.referenced_name FROM ALL_OBJECTS`).WithArgs("APP").
		WillReturnRows(typeDependencyRows().
			AddRow("BASE", nil).AddRow("CHILD", "BASE").AddRow("CHILD", "CHILD").AddRow(`a"type`, "CHILD"))
	mock.ExpectQuery(`SELECT table_name, constraint_name FROM ALL_CONSTRAINTS WHERE owner = :1 AND constraint_type = 'R'`).WithArgs("APP").
		WillReturnRows(sqlmock.NewRows([]string{"table", "constraint"}).AddRow(`a"table`, `a"fk`).AddRow("T", "FK"))
	mock.ExpectExec(`ALTER TABLE "APP"\."a""table" DROP CONSTRAINT "a""fk"$`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`ALTER TABLE "APP"\."T" DROP CONSTRAINT "FK"$`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT object_type, object_name FROM ALL_OBJECTS`).WithArgs("APP", "APP").
		WillReturnRows(sqlmock.NewRows([]string{objectTypeColumn, objectNameColumn}))
	mock.ExpectQuery(`SELECT object_type, object_name FROM ALL_OBJECTS`).WithArgs("APP", "APP").
		WillReturnRows(sqlmock.NewRows([]string{objectTypeColumn, objectNameColumn}).AddRow("TABLE", "T"))
	mock.ExpectExec(`DROP TABLE "APP"\."T" PURGE$`).WillReturnResult(sqlmock.NewResult(0, 0))
	for _, pattern := range []string{`DROP TYPE "APP"\."a""type"$`, `DROP TYPE "APP"\."CHILD"$`, `DROP TYPE "APP"\."BASE"$`} {
		mock.ExpectExec(pattern).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	if err := o.Drop(); err != nil {
		t.Fatal(err)
	}
}

func TestDropRejectsTypeCyclesBeforeDDL(t *testing.T) {
	o, mock := initializedDriver(t)
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM`).WithArgs("APP", "APP", "APP", "APP").
		WillReturnRows(sqlmock.NewRows([]string{countColumn}).AddRow(0))
	mock.ExpectQuery(`SELECT t.object_name, d.referenced_name FROM ALL_OBJECTS`).WithArgs("APP").
		WillReturnRows(typeDependencyRows().AddRow("A", "B").AddRow("B", "A"))
	if err := o.Drop(); err == nil || !strings.Contains(err.Error(), "cyclic") {
		t.Fatalf("type cycle was not rejected before DDL: %v", err)
	}
}

func TestDropPreservesNativeDependencyProtection(t *testing.T) {
	o, mock := initializedDriver(t)
	expectDropPreflight(mock)
	mock.ExpectQuery(`SELECT object_type, object_name FROM ALL_OBJECTS`).WithArgs("APP", "APP").
		WillReturnRows(sqlmock.NewRows([]string{objectTypeColumn, objectNameColumn}))
	mock.ExpectQuery(`SELECT object_type, object_name FROM ALL_OBJECTS`).WithArgs("APP", "APP").
		WillReturnRows(sqlmock.NewRows([]string{objectTypeColumn, objectNameColumn}).AddRow("TABLE", "PARENT"))
	failure := errors.New("ORA-02449: unique/primary keys in table referenced by foreign keys")
	mock.ExpectExec(`DROP TABLE "APP"\."PARENT" PURGE$`).WillReturnError(failure)
	if err := o.Drop(); err == nil || !strings.Contains(err.Error(), failure.Error()) {
		t.Fatalf("native foreign key protection was bypassed: %v", err)
	}
}
