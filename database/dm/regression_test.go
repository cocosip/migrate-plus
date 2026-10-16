package dm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
)

func TestDropRejectsCyclesBeforeAnyDDL(t *testing.T) {
	for _, crossPhase := range []bool{false, true} {
		d, mock := initializedDriver(t)
		expectDropPreflight(mock, 0)
		views, objects := dropObjectRows(), dropObjectRows()
		if crossPhase {
			views.AddRow(1, "MV", "VIEW", 0, 1)
		} else {
			objects.AddRow(1, "F1", "PROC", 0, 0)
		}
		objects.AddRow(2, "F2", "PROC", 0, 0)
		mock.ExpectQuery(`SELECT o.ID, o.NAME, o.SUBTYPE\$`).WithArgs(101, 1).WillReturnRows(views)
		mock.ExpectQuery(`SELECT o.ID, o.NAME, o.SUBTYPE\$`).WithArgs(101, 0).WillReturnRows(objects)
		mock.ExpectQuery(`SELECT d.ID, d.REFED_ID`).WithArgs(101, 101).
			WillReturnRows(dependencyRows().AddRow(1, 2).AddRow(2, 1))
		if err := d.Drop(); err == nil || !strings.Contains(err.Error(), "cyclic") {
			t.Fatalf("cycle was not detected before destructive SQL: %v", err)
		}
	}
}

func TestDropStopsOnPreflightCatalogErrorsBeforeDDL(t *testing.T) {
	for _, stage := range []string{"views", "objects", "dependencies"} {
		d, mock := initializedDriver(t)
		expectDropPreflight(mock, 0)
		failure := errors.New("catalog denied")
		views := mock.ExpectQuery(`SELECT o.ID, o.NAME, o.SUBTYPE\$`).WithArgs(101, 1)
		if stage == "views" {
			views.WillReturnError(failure)
		} else {
			views.WillReturnRows(dropObjectRows())
			objects := mock.ExpectQuery(`SELECT o.ID, o.NAME, o.SUBTYPE\$`).WithArgs(101, 0)
			if stage == "objects" {
				objects.WillReturnError(failure)
			} else {
				objects.WillReturnRows(dropObjectRows())
				mock.ExpectQuery(`SELECT d.ID, d.REFED_ID`).WithArgs(101, 101).WillReturnError(failure)
			}
		}
		if err := d.Drop(); err == nil || !strings.Contains(err.Error(), failure.Error()) {
			t.Fatalf("preflight failure ignored: %v", err)
		}
	}
}

func TestInitializationLockUsesUpstreamTimeout(t *testing.T) {
	previous := migrate.DefaultLockTimeout
	migrate.DefaultLockTimeout = 5 * time.Millisecond
	t.Cleanup(func() { migrate.DefaultLockTimeout = previous })
	db, mock := mockDB(t)
	expectMetadata(mock, 0, 1, 0)
	expectLock(mock, 1)
	mock.ExpectClose()
	if _, err := WithInstance(db, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("initialization did not honor lock timeout: %v", err)
	}
}
