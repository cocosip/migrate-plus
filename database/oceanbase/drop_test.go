package oceanbase

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	ob "github.com/cocosip/go-oceanbase"
)

func TestDropPreflightsBeforeDestruction(t *testing.T) {
	for _, mode := range []ob.Mode{ob.ModeMySQL, ob.ModeOracle} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			d, mock := mockDriver(t, mode)
			expectSession(mock, mode, "APP")
			mock.ExpectQuery(`SELECT COUNT`).WillReturnRows(sqlmock.NewRows([]string{countColumn}).AddRow(1))
			if err := d.Drop(); err == nil {
				t.Fatal("dropped schema with external dependencies")
			}
		})
	}
}

func TestDropCatalogFailureDoesNotExecuteDDL(t *testing.T) {
	d, mock := mockDriver(t, ob.ModeMySQL)
	expectSession(mock, d.mode, "APP")
	mock.ExpectQuery(`SELECT COUNT`).WillReturnError(errors.New("catalog permission denied"))
	if err := d.Drop(); err == nil {
		t.Fatal("ignored catalog error")
	}
}

func TestDropQuotesAndOrdersObjects(t *testing.T) {
	d, mock := mockDriver(t, ob.ModeMySQL)
	expectSession(mock, d.mode, "APP")
	mock.ExpectQuery(`SELECT COUNT`).WillReturnRows(sqlmock.NewRows([]string{countColumn}).AddRow(0))
	mock.ExpectQuery(`SELECT`).WillReturnRows(sqlmock.NewRows([]string{objectNameColumn, objectKindColumn}).AddRow("v", "VIEW").AddRow("a`b", "TABLE").AddRow("p", "PROCEDURE"))
	mock.ExpectQuery(`SELECT`).WillReturnRows(sqlmock.NewRows([]string{"table", "constraint"}).AddRow("a`b", "fk`x"))
	mock.ExpectExec("ALTER TABLE `APP`.`a``b` DROP FOREIGN KEY `fk``x`").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DROP VIEW `APP`.`v`").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DROP TABLE `APP`.`a``b`").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DROP PROCEDURE `APP`.`p`").WillReturnResult(sqlmock.NewResult(0, 0))
	if err := d.Drop(); err != nil {
		t.Fatal(err)
	}
}

func TestDropRejectsUnsupportedObjectBeforeDDL(t *testing.T) {
	d, mock := mockDriver(t, ob.ModeOracle)
	expectSession(mock, d.mode, "APP")
	mock.ExpectQuery(`SELECT COUNT`).WillReturnRows(sqlmock.NewRows([]string{countColumn}).AddRow(0))
	mock.ExpectQuery(`SELECT`).WillReturnRows(sqlmock.NewRows([]string{objectNameColumn, objectKindColumn}).AddRow("t", "TABLE").AddRow("x", "UNSUPPORTED"))
	if err := d.Drop(); err == nil {
		t.Fatal("accepted unknown object type")
	}
}

func TestDropRefusesMaterializedViewsBeforeDDL(t *testing.T) {
	d, mock := mockDriver(t, ob.ModeOracle)
	expectSession(mock, d.mode, "APP")
	mock.ExpectQuery(`SELECT COUNT`).WillReturnRows(sqlmock.NewRows([]string{countColumn}).AddRow(0))
	mock.ExpectQuery(`SELECT`).WillReturnRows(sqlmock.NewRows([]string{objectNameColumn, objectKindColumn}).AddRow("mv", "MATERIALIZED VIEW"))
	if err := d.Drop(); err == nil || !strings.Contains(err.Error(), "unsupported Drop object") {
		t.Fatalf("materialized-view preflight = %v", err)
	}
}
