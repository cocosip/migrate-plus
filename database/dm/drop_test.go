package dm

import (
	"errors"
	"reflect"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func dropObjectRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "name", "subtype", "info", "mv"})
}

func dependencyRows() *sqlmock.Rows { return sqlmock.NewRows([]string{"id", "ref"}) }

func foreignKeyRows() *sqlmock.Rows { return sqlmock.NewRows([]string{"table", "constraint"}) }

func expectDropPreflight(mock sqlmock.Sqlmock, external int) {
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM SYS.SYSDEPENDENCIES`).WithArgs(101, 101).WillReturnRows(sqlmock.NewRows([]string{countColumn}).AddRow(external))
}

func expectEmptyDropOrder(mock sqlmock.Sqlmock) {
	for _, materialized := range []int{1, 0} {
		mock.ExpectQuery(`SELECT o.ID, o.NAME, o.SUBTYPE\$`).WithArgs(101, materialized).
			WillReturnRows(dropObjectRows())
	}
	mock.ExpectQuery(`SELECT d.ID, d.REFED_ID`).WithArgs(101, 101).
		WillReturnRows(dependencyRows())
}

func TestDropUsesNativeSyntaxAndRequeriesAfterMaterializedViews(t *testing.T) {
	d, mock := initializedDriver(t)
	expectDropPreflight(mock, 0)
	mock.ExpectQuery(`SELECT o.ID, o.NAME, o.SUBTYPE\$`).WithArgs(101, 1).
		WillReturnRows(dropObjectRows().AddRow(1, "MV", "VIEW", 0, 1))
	mock.ExpectQuery(`SELECT o.ID, o.NAME, o.SUBTYPE\$`).WithArgs(101, 0).
		WillReturnRows(dropObjectRows().
			AddRow(2, "PARENT", "UTAB", 0, 0).AddRow(3, "CHILD", "UTAB", 0, 0).AddRow(4, `a"view`, "VIEW", 0, 0).AddRow(5, "FN", "PROC", 0, 0).AddRow(6, "P", "PROC", 1, 0))
	mock.ExpectQuery(`SELECT d.ID, d.REFED_ID`).WithArgs(101, 101).
		WillReturnRows(dependencyRows().AddRow(4, 2).AddRow(3, 2))
	mock.ExpectQuery(`SELECT t.NAME, c.NAME FROM SYS.SYSCONS`).WithArgs(101).WillReturnRows(foreignKeyRows().AddRow("CHILD", "FK_PARENT"))
	mock.ExpectExec(`ALTER TABLE "APP"\."CHILD" DROP CONSTRAINT "FK_PARENT"`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT o.ID, o.NAME, o.SUBTYPE\$`).WithArgs(101, 1).WillReturnRows(dropObjectRows().AddRow(1, "MV", "VIEW", 0, 1))
	mock.ExpectQuery(`SELECT d.ID, d.REFED_ID`).WithArgs(101, 101).WillReturnRows(dependencyRows())
	mock.ExpectExec(`DROP MATERIALIZED VIEW "APP"\."MV"`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT o.ID, o.NAME, o.SUBTYPE\$`).WithArgs(101, 0).WillReturnRows(dropObjectRows().
		AddRow(2, "PARENT", "UTAB", 0, 0).AddRow(3, "CHILD", "UTAB", 0, 0).AddRow(4, `a"view`, "VIEW", 0, 0).AddRow(5, "FN", "PROC", 0, 0).AddRow(6, "P", "PROC", 1, 0))
	mock.ExpectQuery(`SELECT d.ID, d.REFED_ID`).WithArgs(101, 101).WillReturnRows(dependencyRows().AddRow(4, 2).AddRow(3, 2))
	for _, query := range []string{`DROP TABLE "APP"\."CHILD" RESTRICT`, `DROP VIEW "APP"\."a""view" RESTRICT`, `DROP FUNCTION "APP"\."FN"`, `DROP PROCEDURE "APP"\."P"`, `DROP TABLE "APP"\."PARENT" RESTRICT`} {
		mock.ExpectExec(query).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	if err := d.Drop(); err != nil {
		t.Fatal(err)
	}
}

func TestDropRejectsCrossSchemaDependenciesBeforeDDL(t *testing.T) {
	d, mock := initializedDriver(t)
	expectDropPreflight(mock, 1)
	if err := d.Drop(); err == nil {
		t.Fatal("accepted external dependency")
	}
}

func TestDropStopsOnCatalogOrConstraintFailure(t *testing.T) {
	for _, catalogError := range []bool{true, false} {
		d, mock := initializedDriver(t)
		expectDropPreflight(mock, 0)
		expectEmptyDropOrder(mock)
		q := mock.ExpectQuery(`SELECT t.NAME, c.NAME FROM SYS.SYSCONS`).WithArgs(101)
		if catalogError {
			q.WillReturnError(errors.New("denied"))
		} else {
			q.WillReturnRows(foreignKeyRows().AddRow("T", "FK"))
			mock.ExpectExec(`ALTER TABLE`).WillReturnError(errors.New("denied"))
		}
		if err := d.Drop(); err == nil {
			t.Fatal("ignored Drop failure")
		}
	}
}

func TestDependencyOrderingRejectsCyclesAndIgnoresSelfEdges(t *testing.T) {
	objects := []dropObject{{id: 1}, {id: 2}, {id: 3}}
	got, err := orderObjects(objects, [][2]int64{{1, 1}, {2, 1}, {3, 2}})
	if err != nil || !reflect.DeepEqual(got, []dropObject{{id: 3}, {id: 2}, {id: 1}}) {
		t.Fatalf("order: %+v %v", got, err)
	}
	if _, err := orderObjects(objects, [][2]int64{{1, 2}, {2, 1}}); err == nil {
		t.Fatal("accepted dependency cycle")
	}
}

func TestDropPreflightAllowsLocalForeignKeyConstraintCycles(t *testing.T) {
	d, mock := initializedDriver(t)
	expectDropPreflight(mock, 0)
	mock.ExpectQuery(`SELECT o.ID, o.NAME, o.SUBTYPE\$`).WithArgs(101, 1).WillReturnRows(dropObjectRows())
	mock.ExpectQuery(`SELECT o.ID, o.NAME, o.SUBTYPE\$`).WithArgs(101, 0).
		WillReturnRows(dropObjectRows().AddRow(1, "PARENT", "UTAB", 0, 0).AddRow(2, "CHILD", "UTAB", 0, 0))
	// Foreign-key constraint IDs are not independently dropped schema objects.
	mock.ExpectQuery(`SELECT d.ID, d.REFED_ID`).WithArgs(101, 101).
		WillReturnRows(dependencyRows().AddRow(11, 1).AddRow(11, 2).AddRow(12, 2).AddRow(12, 1))
	mock.ExpectQuery(`SELECT t.NAME, c.NAME FROM SYS.SYSCONS`).WithArgs(101).
		WillReturnRows(foreignKeyRows().AddRow("CHILD", "FK_PARENT").AddRow("PARENT", "FK_CHILD"))
	for _, query := range []string{`ALTER TABLE "APP"\."CHILD" DROP CONSTRAINT "FK_PARENT"`, `ALTER TABLE "APP"\."PARENT" DROP CONSTRAINT "FK_CHILD"`} {
		mock.ExpectExec(query).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	mock.ExpectQuery(`SELECT o.ID, o.NAME, o.SUBTYPE\$`).WithArgs(101, 1).WillReturnRows(dropObjectRows())
	mock.ExpectQuery(`SELECT d.ID, d.REFED_ID`).WithArgs(101, 101).WillReturnRows(dependencyRows())
	mock.ExpectQuery(`SELECT o.ID, o.NAME, o.SUBTYPE\$`).WithArgs(101, 0).
		WillReturnRows(dropObjectRows().AddRow(1, "PARENT", "UTAB", 0, 0).AddRow(2, "CHILD", "UTAB", 0, 0))
	mock.ExpectQuery(`SELECT d.ID, d.REFED_ID`).WithArgs(101, 101).WillReturnRows(dependencyRows())
	for _, query := range []string{`DROP TABLE "APP"\."PARENT" RESTRICT`, `DROP TABLE "APP"\."CHILD" RESTRICT`} {
		mock.ExpectExec(query).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	if err := d.Drop(); err != nil {
		t.Fatal(err)
	}
}
