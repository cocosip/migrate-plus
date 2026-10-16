package dm

import (
	"errors"
	"fmt"
	"testing"
	"testing/fstest"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

func existingStateDriver(t *testing.T, columnType string, dirty any) (*Dm, sqlmock.Sqlmock) {
	t.Helper()
	db, mock := mockDB(t)
	expectMetadata(mock, 0, 1, 0)
	expectLock(mock, 0)
	mock.ExpectQuery(`SELECT COUNT`).WillReturnRows(sqlmock.NewRows([]string{countColumn}).AddRow(1))
	mock.ExpectQuery(`SELECT "VERSION", "DIRTY".*WHERE 1 = 0`).WillReturnRows(sqlmock.NewRowsWithColumnDefinition(
		sqlmock.NewColumn(versionColumn).OfType(bigintType, int64(0)),
		sqlmock.NewColumn(dirtyColumn).OfType(columnType, dirty).WithPrecisionAndScale(19, 0)))
	expectState(mock, 7, dirty)
	expectUnlock(mock, 0)
	d, err := WithInstance(db, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		mock.ExpectClose()
		if err := d.Close(); err != nil {
			t.Error(err)
		}
	})
	return d.(*Dm), mock
}

func expectState(mock sqlmock.Sqlmock, version int, dirty any) {
	mock.ExpectQuery(`SELECT "VERSION", "DIRTY" FROM "APP"\."SCHEMA_MIGRATIONS"$`).
		WillReturnRows(mock.NewRows([]string{versionColumn, dirtyColumn}).AddRow(version, dirty)).RowsWillBeClosed()
}

func expectSetState(mock sqlmock.Sqlmock, version, dirty int) {
	mock.ExpectBegin()
	mock.ExpectExec(`^DELETE FROM "APP"\."SCHEMA_MIGRATIONS"$`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO .* \("VERSION", "DIRTY"\) VALUES \(\?, \?\)`).WithArgs(version, dirty).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
}

func TestExistingVersionTablePreservesState(t *testing.T) {
	for _, columnType := range []string{bitType, "TINYINT", smallintType, "INT", integerType, bigintType, numberType, "NUMERIC", decimalType, "DEC"} {
		for _, dirty := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/%d", columnType, dirty), func(t *testing.T) {
				d, mock := existingStateDriver(t, columnType, dirty)
				expectState(mock, 7, dirty)
				if v, gotDirty, err := d.Version(); err != nil || v != 7 || gotDirty != (dirty == 1) {
					t.Fatalf("existing state = %d, %v, %v", v, gotDirty, err)
				}
			})
		}
	}
}

func TestVersionRejectsInvalidValuesAndReturnsErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version any
		dirty   any
	}{
		{"null dirty", 7, nil}, {"null version", nil, 0}, {"bad version", "bad", 0},
		{"negative dirty", 7, -1}, {"large dirty", 7, 2}, {"float zero", 7, float64(0)},
		{"float one", 7, float64(1)}, {"fraction", 7, 0.5}, {"text bool", 7, "true"},
		{"text number", 7, "2"}, {"padded number", 7, " 1"}, {"bytes", 7, []byte("1")},
		{"time", 7, time.Now()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, mock := initializedDriver(t)
			mock.ExpectQuery(`SELECT "VERSION", "DIRTY"`).WillReturnRows(
				sqlmock.NewRows([]string{versionColumn, dirtyColumn}).AddRow(tc.version, tc.dirty)).RowsWillBeClosed()
			var operation database.Error
			if _, _, err := d.Version(); !errors.As(err, &operation) {
				t.Fatalf("invalid state did not return database error: %v", err)
			}
		})
	}
	for _, stage := range []string{"query", "first row", "second row", "row close error"} {
		t.Run(stage, func(t *testing.T) {
			d, mock := initializedDriver(t)
			failure := errors.New("state read failed")
			query := mock.ExpectQuery(`SELECT "VERSION", "DIRTY"`)
			switch stage {
			case "query":
				query.WillReturnError(failure)
			case "first row":
				query.WillReturnRows(sqlmock.NewRows([]string{versionColumn, dirtyColumn}).AddRow(7, 0).RowError(0, failure))
			case "second row":
				query.WillReturnRows(sqlmock.NewRows([]string{versionColumn, dirtyColumn}).AddRow(7, 0).AddRow(8, 0).RowError(1, failure))
			case "row close error":
				query.WillReturnRows(sqlmock.NewRows([]string{versionColumn, dirtyColumn}).AddRow(7, 0).CloseError(failure))
			}
			var operation database.Error
			if _, _, err := d.Version(); !errors.As(err, &operation) || !errors.Is(operation.OrigErr, failure) {
				t.Fatalf("lost state read error: %v", err)
			}
		})
	}
}

func TestVersionAcceptsNativeDirtyStates(t *testing.T) {
	d, mock := initializedDriver(t)
	for _, tc := range []struct {
		value any
		want  bool
	}{
		{false, false}, {true, true}, {"0", false}, {"1", true},
		{int8(0), false}, {int8(1), true}, {int16(0), false}, {int16(1), true},
		{int32(0), false}, {int32(1), true}, {int64(0), false}, {int64(1), true},
	} {
		t.Run(fmt.Sprintf("%T/%v", tc.value, tc.value), func(t *testing.T) {
			expectState(mock, 7, tc.value)
			if v, dirty, err := d.Version(); err != nil || v != 7 || dirty != tc.want {
				t.Fatalf("state = %d, %v, %v", v, dirty, err)
			}
		})
	}
}

func TestVersionColumnRequirements(t *testing.T) {
	for _, tc := range []struct {
		versionType string
		precision   int64
		scale       int64
		dirtyType   string
		valid       bool
	}{
		{bitType, 0, 0, bitType, false}, {"TINYINT", 0, 0, bitType, false}, {smallintType, 0, 0, bitType, false},
		{"INT", 0, 0, bitType, false}, {integerType, 0, 0, bitType, false}, {varcharType, 0, 0, bitType, false},
		{numberType, 18, 0, bitType, false}, {decimalType, 19, 1, bitType, false}, {bigintType, 0, 0, varcharType, false},
		{bigintType, 0, 0, bitType, true}, {numberType, 19, 0, bitType, true}, {"NUMERIC", 19, 0, bitType, true},
		{decimalType, 19, 0, bitType, true}, {"DEC", 19, 0, bitType, true},
	} {
		t.Run(fmt.Sprintf("%s/%d/%d/%s", tc.versionType, tc.precision, tc.scale, tc.dirtyType), func(t *testing.T) {
			d, mock := initializedDriver(t)
			mock.ExpectQuery("column probe").WillReturnRows(sqlmock.NewRowsWithColumnDefinition(
				sqlmock.NewColumn(versionColumn).OfType(tc.versionType, int64(0)).WithPrecisionAndScale(tc.precision, tc.scale),
				sqlmock.NewColumn(dirtyColumn).OfType(tc.dirtyType, int64(0))))
			if err := d.query("column probe", validateVersionColumns); (err == nil) != tc.valid {
				t.Fatalf("column validation = %v; valid = %v", err, tc.valid)
			}
		})
	}
}

func TestExistingVersionTableUpAndSetVersion(t *testing.T) {
	for _, columnType := range []string{bitType, smallintType} {
		for _, dirty := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/%d", columnType, dirty), func(t *testing.T) {
				d, mock := existingStateDriver(t, columnType, dirty)
				source, err := iofs.New(fstest.MapFS{
					"7_old.up.sql":  &fstest.MapFile{Data: []byte("DO NOT REPLAY")},
					"8_next.up.sql": &fstest.MapFile{Data: []byte("INSERT INTO probe VALUES(8);")},
				}, ".")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = source.Close() })
				m, err := migrate.NewWithInstance("iofs", source, "dm", d)
				if err != nil {
					t.Fatal(err)
				}
				expectLock(mock, 0)
				expectState(mock, 7, dirty)
				if dirty == 1 {
					expectUnlock(mock, 0)
					var dirtyErr migrate.ErrDirty
					if err := m.Up(); !errors.As(err, &dirtyErr) || dirtyErr.Version != 7 {
						t.Fatalf("Up lost dirty history: %v", err)
					}
					return
				}
				expectSetState(mock, 8, 1)
				mock.ExpectExec(`^INSERT INTO probe VALUES\(8\)$`).WillReturnResult(sqlmock.NewResult(0, 1))
				expectSetState(mock, 8, 0)
				expectUnlock(mock, 0)
				if err := m.Up(); err != nil {
					t.Fatal(err)
				}
				expectLock(mock, 0)
				expectState(mock, 8, 0)
				expectUnlock(mock, 0)
				if err := m.Up(); !errors.Is(err, migrate.ErrNoChange) {
					t.Fatalf("repeated Up = %v", err)
				}
				for _, state := range []int{1, 0} {
					expectSetState(mock, 9, state)
					if err := d.SetVersion(9, state == 1); err != nil {
						t.Fatal(err)
					}
					expectState(mock, 9, state)
					if v, gotDirty, err := d.Version(); err != nil || v != 9 || gotDirty != (state == 1) {
						t.Fatalf("round trip = %d, %v, %v", v, gotDirty, err)
					}
				}
			})
		}
	}
}
