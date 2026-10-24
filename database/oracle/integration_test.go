//go:build integration

package oracle

import (
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	databasetesting "github.com/golang-migrate/migrate/v4/database/testing"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// This test destroys schema objects. The URL must identify a disposable schema.
func TestIntegration(t *testing.T) {
	raw := os.Getenv("MIGRATE_PLUS_ORACLE_TEST_URL")
	if raw == "" {
		t.Skip("set MIGRATE_PLUS_ORACLE_TEST_URL to a disposable Oracle schema; tests destroy its objects")
	}
	t.Run("upstream conformance", func(t *testing.T) {
		d := openTestDriver(t, raw)
		databasetesting.Test(t, d, []byte("CREATE TABLE migrate_plus_probe (id NUMBER);"))
	})
	t.Run("drop orders dependent types and cyclic local foreign keys", func(t *testing.T) {
		d := openTestDriver(t, raw)
		if err := d.Run(strings.NewReader(`CREATE TYPE migrate_plus_base AS OBJECT (id NUMBER);
/
CREATE TYPE migrate_plus_child_type AS OBJECT (item migrate_plus_base);
/
CREATE TABLE migrate_plus_parent(id NUMBER PRIMARY KEY, child_id NUMBER, item migrate_plus_child_type);
CREATE TABLE migrate_plus_child(id NUMBER PRIMARY KEY, parent_id NUMBER REFERENCES migrate_plus_parent(id));
ALTER TABLE migrate_plus_parent ADD CONSTRAINT migrate_plus_reverse_fk FOREIGN KEY(child_id) REFERENCES migrate_plus_child(id);`)); err != nil {
			t.Fatal(err)
		}
		if err := d.Drop(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("cross schema foreign key survives rejected Drop", func(t *testing.T) { integrationCrossSchema(t, raw) })
	t.Run("drop removes ordinary and prebuilt materialized view tables", func(t *testing.T) {
		d := openTestDriver(t, raw)
		if err := d.Run(strings.NewReader(`CREATE TABLE migrate_plus_source (id NUMBER);
INSERT INTO migrate_plus_source VALUES (1);
CREATE TABLE migrate_plus_prebuilt AS SELECT id FROM migrate_plus_source;
CREATE MATERIALIZED VIEW migrate_plus_prebuilt ON PREBUILT TABLE
REFRESH COMPLETE ON DEMAND AS SELECT id FROM migrate_plus_source;
CREATE MATERIALIZED VIEW migrate_plus_regular
REFRESH COMPLETE ON DEMAND AS SELECT id FROM migrate_plus_source;`)); err != nil {
			t.Fatal(err)
		}
		if err := d.Drop(); err != nil {
			t.Fatal(err)
		}
		o := d.(*Oracle)
		ctx, cancel := o.operationContext()
		defer cancel()
		var count int
		if err := o.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM ALL_OBJECTS
WHERE owner = :1 AND object_name IN ('MIGRATE_PLUS_SOURCE', 'MIGRATE_PLUS_PREBUILT', 'MIGRATE_PLUS_REGULAR')
AND object_type IN ('TABLE', 'MATERIALIZED VIEW')`, o.schema).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("drop left %d tables or materialized views", count)
		}
	})
	t.Run("lock survives DDL and close releases it", func(t *testing.T) {
		first, second := openTestDriver(t, raw), openTestDriver(t, raw)
		if err := first.Lock(); err != nil {
			t.Fatal(err)
		}
		assertLocked(t, second.(*Oracle))
		if err := first.Run(strings.NewReader("CREATE TABLE migrate_plus_lock_probe (id NUMBER);")); err != nil {
			t.Fatal(err)
		}
		assertLocked(t, second.(*Oracle))
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		if err := second.Lock(); err != nil {
			t.Fatalf("close retained session lock: %v", err)
		}
		if err := second.Drop(); err != nil {
			t.Fatal(err)
		}
		if err := second.Unlock(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("upstream up dirty force down", func(t *testing.T) {
		d := openTestDriver(t, raw)
		source, err := iofs.New(fstest.MapFS{
			"1_init.up.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE migrate_plus_items (id NUMBER PRIMARY KEY);
CREATE SEQUENCE migrate_plus_seq;
CREATE OR REPLACE PROCEDURE migrate_plus_noop AS BEGIN NULL; END;
/
INSERT INTO migrate_plus_items VALUES (migrate_plus_seq.NEXTVAL);`)},
			"1_init.down.sql":    &fstest.MapFile{Data: []byte("DROP PROCEDURE migrate_plus_noop; DROP SEQUENCE migrate_plus_seq; DROP TABLE migrate_plus_items PURGE;")},
			"2_failure.up.sql":   &fstest.MapFile{Data: []byte("INSERT INTO migrate_plus_missing VALUES (1);")},
			"2_failure.down.sql": &fstest.MapFile{Data: []byte("BEGIN NULL; END;\n/\n")},
		}, ".")
		if err != nil {
			t.Fatal(err)
		}
		m, err := migrate.NewWithInstance("iofs", source, "oracle", d)
		if err != nil {
			_ = source.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if sourceErr, dbErr := m.Close(); sourceErr != nil || dbErr != nil {
				t.Errorf("migrator close: %v, %v", sourceErr, dbErr)
			}
		})
		if err := m.Steps(1); err != nil {
			t.Fatal(err)
		}
		if err := m.Steps(1); err == nil {
			t.Fatal("invalid migration succeeded")
		}
		persisted := openTestDriver(t, raw)
		version, dirty, err := persisted.Version()
		if err != nil || version != 2 || !dirty {
			t.Fatalf("dirty state was not durable: %d, %v, %v", version, dirty, err)
		}
		var dirtyErr migrate.ErrDirty
		if err := m.Up(); !errors.As(err, &dirtyErr) {
			t.Fatalf("upstream accepted dirty state: %v", err)
		}
		if err := m.Force(1); err != nil {
			t.Fatal(err)
		}
		if err := m.Down(); err != nil {
			t.Fatal(err)
		}
		if version, dirty, err := d.Version(); err != nil || version != database.NilVersion || dirty {
			t.Fatalf("down state = %d, %v, %v", version, dirty, err)
		}
		if err := d.Drop(); err != nil {
			t.Fatal(err)
		}
	})
}

func integrationCrossSchema(t *testing.T, raw string) {
	otherURL := os.Getenv("MIGRATE_PLUS_ORACLE_OTHER_TEST_URL")
	if otherURL == "" {
		t.Skip("set MIGRATE_PLUS_ORACLE_OTHER_TEST_URL to another disposable schema on the same server")
	}
	first, other := openTestDriver(t, raw).(*Oracle), openTestDriver(t, otherURL).(*Oracle)
	if first.schema == other.schema {
		t.Fatal("cross-schema test requires two distinct schemas")
	}
	if err := first.exec(`CREATE TABLE migrate_plus_external (id NUMBER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.exec(`DROP TABLE migrate_plus_external PURGE`) })
	if err := first.exec(`GRANT REFERENCES ON migrate_plus_external TO ` + quoteIdentifier(other.schema)); err != nil {
		t.Fatal(err)
	}
	if err := other.exec(`CREATE TABLE migrate_plus_external_child (parent_id NUMBER,
CONSTRAINT migrate_plus_external_fk FOREIGN KEY (parent_id) REFERENCES ` + quoteIdentifier(first.schema) + `.migrate_plus_external(id))`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.exec(`DROP TABLE migrate_plus_external_child PURGE`) })
	if err := first.Drop(); err == nil {
		t.Fatal("Drop accepted another schema's foreign key")
	}
	ctx, cancel := other.operationContext()
	defer cancel()
	var status string
	if err := other.conn.QueryRowContext(ctx, `SELECT status FROM ALL_CONSTRAINTS
WHERE owner = :1 AND constraint_name = 'MIGRATE_PLUS_EXTERNAL_FK'`, other.schema).Scan(&status); err != nil || status != "ENABLED" {
		t.Fatalf("Drop changed the external foreign key: status=%q err=%v", status, err)
	}
	if err := other.exec(`DROP TABLE migrate_plus_external_child PURGE`); err != nil {
		t.Fatal(err)
	}
	if err := first.Drop(); err != nil {
		t.Fatal(err)
	}
}

func assertLocked(t *testing.T, o *Oracle) {
	t.Helper()
	// Probe contention without timing out and invalidating the contender session.
	code := -1
	err := o.exec(`BEGIN :1 := DBMS_LOCK.REQUEST(id => :2, lockmode => 6, timeout => 0, release_on_commit => FALSE); END;`, sql.Out{Dest: &code}, o.lockID)
	if err != nil || code != 1 {
		t.Fatalf("native lock contention = %d, %v", code, err)
	}
}

func openTestDriver(t *testing.T, raw string) database.Driver {
	t.Helper()
	d, err := database.Open(raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Error(err)
		}
	})
	return d
}