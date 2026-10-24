//go:build integration

package oceanbase

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	ob "github.com/cocosip/go-oceanbase"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	databasetesting "github.com/golang-migrate/migrate/v4/database/testing"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// These tests destroy supported objects in the explicitly selected disposable schemas.
// Each compatibility mode has its own variable so one target never implies the other.
func TestIntegrationMySQL(t *testing.T) {
	runIntegration(t, "MIGRATE_PLUS_OCEANBASE_MYSQL_TEST_URL", ob.ModeMySQL)
}

func TestIntegrationOracle(t *testing.T) {
	runIntegration(t, "MIGRATE_PLUS_OCEANBASE_ORACLE_TEST_URL", ob.ModeOracle)
}

func runIntegration(t *testing.T, env string, mode ob.Mode) {
	raw := os.Getenv(env)
	if raw == "" {
		t.Skipf("set %s to a disposable OceanBase schema; tests destroy its objects", env)
	}
	if got := openTestDriver(t, raw).mode; got != mode {
		t.Fatalf("%s targets mode %v, want %v", env, got, mode)
	}
	probe := []byte("CREATE TABLE migrate_plus_probe(id BIGINT)")
	t.Run("upstream conformance", func(t *testing.T) {
		databasetesting.Test(t, openTestDriver(t, raw), probe)
	})
	t.Run("lock contention and survival", func(t *testing.T) { integrationLock(t, raw) })
	t.Run("durable state", func(t *testing.T) { integrationDurability(t, raw) })
	t.Run("dirty force and down", func(t *testing.T) { integrationDirtyRecovery(t, raw) })
	t.Run("timeout discards session", func(t *testing.T) { integrationTimeout(t, raw) })
	t.Run("reopen existing history", func(t *testing.T) { integrationReopen(t, raw) })
	t.Run("Drop removes dependent objects", func(t *testing.T) { integrationDrop(t, raw) })
}

func integrationLock(t *testing.T, raw string) {
	first, second := openTestDriver(t, raw), openTestDriver(t, raw)
	if err := first.Lock(); err != nil {
		t.Fatal(err)
	}
	mustExec(t, first, "CREATE TABLE migrate_plus_lock_probe(id BIGINT)")
	assertLocked(t, second)
	if err := first.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := second.Lock(); err != nil {
		t.Fatalf("Unlock retained the lock: %v", err)
	}
	if err := second.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := first.Lock(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Lock(); err != nil {
		t.Fatalf("Close retained session lock: %v", err)
	}
	if err := second.Drop(); err != nil {
		t.Fatal(err)
	}
}

func integrationDurability(t *testing.T, raw string) {
	first, observer := openTestDriver(t, raw), openTestDriver(t, raw)
	mustExec(t, first, "CREATE TABLE migrate_plus_durable(id BIGINT)")
	for _, step := range []struct {
		version int
		dirty   bool
	}{{1, true}, {1, false}, {database.NilVersion, false}} {
		if err := first.SetVersion(step.version, step.dirty); err != nil {
			t.Fatal(err)
		}
		if v, dirty, err := observer.Version(); err != nil || v != step.version || dirty != step.dirty {
			t.Fatalf("observed state = %d %v %v, want %d %v", v, dirty, err, step.version, step.dirty)
		}
	}
	if err := first.Drop(); err != nil {
		t.Fatal(err)
	}
}

func integrationDirtyRecovery(t *testing.T, raw string) {
	d := openTestDriver(t, raw)
	source, err := iofs.New(fstest.MapFS{
		"1_init.up.sql":   &fstest.MapFile{Data: []byte("CREATE TABLE migrate_plus_items(id BIGINT PRIMARY KEY)")},
		"1_init.down.sql": &fstest.MapFile{Data: []byte("DROP TABLE migrate_plus_items")},
		"2_fail.up.sql":   &fstest.MapFile{Data: []byte("SELECT * FROM migrate_plus_missing")},
		"2_fail.down.sql": &fstest.MapFile{Data: []byte("CREATE TABLE migrate_plus_ignored(id BIGINT)")},
	}, ".")
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.NewWithInstance("iofs", source, "oceanbase", d)
	if err != nil {
		_ = source.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if srcErr, dbErr := m.Close(); srcErr != nil || dbErr != nil {
			t.Errorf("migrator close: %v, %v", srcErr, dbErr)
		}
	})
	if err := m.Steps(1); err != nil {
		t.Fatal(err)
	}
	if err := m.Steps(1); err == nil {
		t.Fatal("failing migration succeeded")
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
	if v, dirty, err := d.Version(); err != nil || v != database.NilVersion || dirty {
		t.Fatalf("down state = %d %v %v", v, dirty, err)
	}
	if err := d.Drop(); err != nil {
		t.Fatal(err)
	}
}

func integrationTimeout(t *testing.T, raw string) {
	d, contender := openTestDriver(t, raw), openTestDriver(t, raw)
	if err := d.Lock(); err != nil {
		t.Fatal(err)
	}
	sleep := "SELECT SLEEP(5)"
	if d.mode == ob.ModeOracle {
		sleep = "BEGIN DBMS_LOCK.SLEEP(5); END;"
	}
	d.config.StatementTimeout = 200 * time.Millisecond
	if err := d.Run(strings.NewReader(sleep)); err == nil || !d.broken.Load() {
		t.Fatalf("timeout did not discard session: %v", err)
	}
	contender.config.StatementTimeout = 10 * time.Second
	if err := contender.Lock(); err != nil {
		t.Fatalf("discarded session retained lock: %v", err)
	}
	if err := contender.Drop(); err != nil {
		t.Fatal(err)
	}
}

func integrationReopen(t *testing.T, raw string) {
	first := openTestDriver(t, raw)
	if err := first.SetVersion(7, true); err != nil {
		t.Fatal(err)
	}
	second := openTestDriver(t, raw)
	if v, dirty, err := second.Version(); err != nil || v != 7 || !dirty {
		t.Fatalf("reopened history = %d %v %v; existing dirty state must be preserved", v, dirty, err)
	}
	if err := second.Drop(); err != nil {
		t.Fatal(err)
	}
}

func integrationDrop(t *testing.T, raw string) {
	d := openTestDriver(t, raw)
	mustExec(t, d, `CREATE TABLE migrate_plus_parent(id BIGINT PRIMARY KEY);
CREATE TABLE migrate_plus_child(id BIGINT PRIMARY KEY, parent_id BIGINT,
CONSTRAINT migrate_plus_fk FOREIGN KEY(parent_id) REFERENCES migrate_plus_parent(id));
CREATE VIEW migrate_plus_view AS SELECT id FROM migrate_plus_parent`)
	if err := d.Drop(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	objects, err := d.dropObjects(ctx)
	if err != nil || len(objects) != 0 {
		t.Fatalf("remaining objects = %v: %v", objects, err)
	}
}

func openTestDriver(t *testing.T, raw string) *OceanBase {
	t.Helper()
	opened, err := database.Open(raw)
	if err != nil {
		t.Fatal(err)
	}
	d := opened.(*OceanBase)
	t.Logf("mode=%v version=%s schema=%s sql_mode=%q", d.mode, d.serverVersion, d.schema, d.sqlMode)
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Error(err)
		}
	})
	return d
}

func mustExec(t *testing.T, d *OceanBase, script string) {
	t.Helper()
	if err := d.Run(strings.NewReader(script)); err != nil {
		t.Fatal(err)
	}
}

// assertLocked proves that a second session cannot take the same native lock.
func assertLocked(t *testing.T, d *OceanBase) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	code, err := d.requestLock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	acquired := d.mode == ob.ModeMySQL && code == 1 || d.mode == ob.ModeOracle && code == 0
	if acquired {
		_ = d.unlock()
		t.Fatal("another session acquired the migration lock")
	}
}