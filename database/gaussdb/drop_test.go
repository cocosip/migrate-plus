package gaussdb

import (
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func emptyObjects() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"classid", "id", "name", "object_kind", "args"})
}

func emptyDependencies() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"object_class", "id", "ref_class", "ref_id", "kind", "local", "ref_local"})
}

func expectDropCatalogs(mock sqlmock.Sqlmock, objects, dependencies *sqlmock.Rows) {
	mock.ExpectQuery(`SELECT 'pg_catalog.pg_class'::regclass::oid::bigint`).WithArgs(int64(42)).WillReturnRows(objects)
	mock.ExpectQuery(`SELECT c.tableoid::bigint, c.oid::bigint, t.relname, c.conname`).WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"class", "id", "table", "constraint"}))
	mock.ExpectQuery(`WITH RECURSIVE scoped`).WithArgs(int64(42)).WillReturnRows(dependencies)
}

func TestDropOrdersDependenciesQuotesNamesAndNeverCascades(t *testing.T) {
	g, mock := initializedDriver(t)
	objects := emptyObjects().AddRow(1259, 1, `t"x`, "TABLE", "").AddRow(1259, 2, "v", "VIEW", "")
	deps := emptyDependencies().AddRow(1259, 2, 1259, 1, "n", true, true)
	expectDropCatalogs(mock, objects, deps)
	mock.ExpectExec(`DROP VIEW "app"\."v" RESTRICT`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DROP TABLE "app"\."t""x" RESTRICT`).WillReturnResult(sqlmock.NewResult(0, 0))
	if err := g.Drop(); err != nil {
		t.Fatal(err)
	}
}

func TestDropRejectsExternalDependenciesUnsupportedObjectsAndCyclesBeforeDDL(t *testing.T) {
	for _, tc := range []struct {
		name string
		deps *sqlmock.Rows
	}{
		{"external", emptyDependencies().AddRow(2618, 9, 1259, 1, "n", false, true)},
		{"external owned", emptyDependencies().AddRow(1259, 1, 2615, 99, "x", true, false)},
		{"unsupported", emptyDependencies().AddRow(9999, 9, 0, 0, "s", true, false)},
		{"extension", emptyDependencies().AddRow(1259, 1, 3079, 9, "e", true, false)},
		{"cycle", emptyDependencies().AddRow(1259, 1, 1259, 2, "n", true, true).AddRow(1259, 2, 1259, 1, "n", true, true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, mock := initializedDriver(t)
			expectDropCatalogs(mock, emptyObjects().AddRow(1259, 1, "a", "TABLE", "").AddRow(1259, 2, "b", "VIEW", ""), tc.deps)
			if err := g.Drop(); err == nil || !strings.HasPrefix(err.Error(), "gaussdb:") {
				t.Fatalf("unsafe Drop was not rejected before DDL: %v", err)
			}
		})
	}
}

func TestDropMapsViewRewriteDependenciesAndRoutineOverloads(t *testing.T) {
	g, mock := initializedDriver(t)
	objects := emptyObjects().AddRow(1259, 1, "t", "TABLE", "").AddRow(1259, 2, "v", "VIEW", "").AddRow(1255, 3, "f", "FUNCTION", "integer")
	deps := emptyDependencies().AddRow(2618, 8, 0, 0, "s", true, false).
		AddRow(2618, 8, 1259, 2, "i", true, true).AddRow(2618, 8, 1259, 1, "n", true, true)
	expectDropCatalogs(mock, objects, deps)
	mock.ExpectExec(`DROP VIEW "app"\."v" RESTRICT`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DROP FUNCTION "app"\."f"\(integer\) RESTRICT`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DROP TABLE "app"\."t" RESTRICT`).WillReturnResult(sqlmock.NewResult(0, 0))
	if err := g.Drop(); err != nil {
		t.Fatal(err)
	}
}

func TestDropRelistsAfterMaterializedViewsAndStopsOnErrors(t *testing.T) {
	g, mock := initializedDriver(t)
	expectDropCatalogs(mock, emptyObjects().AddRow(1259, 1, "cache", "MATERIALIZED VIEW", ""), emptyDependencies())
	mock.ExpectExec(`DROP MATERIALIZED VIEW "app"\."cache" RESTRICT`).WillReturnResult(sqlmock.NewResult(0, 0))
	expectDropCatalogs(mock, emptyObjects().AddRow(1259, 2, "retained", "TABLE", ""), emptyDependencies())
	mock.ExpectExec(`DROP TABLE`).WillReturnError(assertDropError{})
	if err := g.Drop(); err == nil || !strings.Contains(err.Error(), "drop denied") {
		t.Fatalf("Drop failure = %v", err)
	}
}

func TestDropRemovesLocalForeignKeysAndOwnedSequenceDependencies(t *testing.T) {
	g, mock := initializedDriver(t)
	mock.ExpectQuery(`SELECT 'pg_catalog.pg_class'::regclass::oid::bigint`).WithArgs(int64(42)).
		WillReturnRows(emptyObjects().AddRow(1259, 1, "parent", "TABLE", "").AddRow(1259, 2, "child", "TABLE", ""))
	mock.ExpectQuery(`SELECT c.tableoid::bigint`).WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"class", "id", "table", "constraint"}).AddRow(2606, 3, "child", "parent_fk"))
	mock.ExpectQuery(`WITH RECURSIVE scoped`).WithArgs(int64(42)).WillReturnRows(emptyDependencies().
		AddRow(2606, 3, 1259, 2, "a", true, true).AddRow(2606, 3, 1259, 1, "n", true, true).
		AddRow(1259, 4, 1259, 1, "a", true, true).AddRow(2604, 5, 1259, 1, "a", true, true).
		AddRow(2604, 5, 1259, 4, "n", true, true))
	mock.ExpectExec(`ALTER TABLE "app"\."child" DROP CONSTRAINT "parent_fk" RESTRICT`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DROP TABLE "app"\."parent" RESTRICT`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`DROP TABLE "app"\."child" RESTRICT`).WillReturnResult(sqlmock.NewResult(0, 0))
	if err := g.Drop(); err != nil {
		t.Fatal(err)
	}
}

type assertDropError struct{}

func (assertDropError) Error() string { return "drop denied" }
