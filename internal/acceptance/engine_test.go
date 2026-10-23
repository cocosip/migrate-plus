package acceptance

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// This driver checks runner assumptions about upstream control flow without a server.
type engineStore struct {
	mu            sync.Mutex
	version       int
	dirty, locked bool
	fail          string
	lockDelay     time.Duration
}

func (d *engineStore) Open(string) (database.Driver, error) { return d, nil }
func (d *engineStore) Close() error                         { return nil }
func (d *engineStore) Drop() error                          { return nil }
func (d *engineStore) Lock() error {
	if d.lockDelay != 0 {
		time.Sleep(d.lockDelay)
		return context.DeadlineExceeded
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.locked {
		return database.ErrLocked
	}
	d.locked = true
	return nil
}

func (d *engineStore) Unlock() error { d.mu.Lock(); defer d.mu.Unlock(); d.locked = false; return nil }
func (d *engineStore) SetVersion(v int, dirty bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.version, d.dirty = v, dirty
	return nil
}
func (d *engineStore) Version() (int, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.version, d.dirty, nil
}
func (d *engineStore) Run(r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if d.fail != "" && strings.Contains(string(data), d.fail) {
		return errors.New("expected scripted execution failure")
	}
	return nil
}

func newEngine(t *testing.T, files fstest.MapFS, d *engineStore) *migrate.Migrate {
	t.Helper()
	src, err := iofs.New(files, ".")
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "contract", d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a, b := m.Close()
		if a != nil || b != nil {
			t.Errorf("close=%v/%v", a, b)
		}
	})
	return m
}

func TestUpstreamPartialStepsAndDeterministicStop(t *testing.T) {
	p, _ := profileFor(driverDM, modePG)
	files, err := copyFixtures(p.FS)
	if err != nil {
		t.Fatal(err)
	}
	d := &engineStore{version: -1}
	m := newEngine(t, files, d)
	m.Log = &stopLogger{stop: m.GracefulStop}
	if err := m.Up(); err != nil {
		t.Fatal(err)
	}
	if v, dirty, _ := d.Version(); v != 0 || dirty {
		t.Fatalf("stop after first migration=%d/%v", v, dirty)
	}
	m.Log = nil
	// GracefulStop latches in upstream; the runner resumes with a fresh migrator.
	m = newEngine(t, files, d)
	var short migrate.ErrShortLimit
	if err := m.Steps(99); !errors.As(err, &short) || short.Short != 95 {
		t.Fatalf("remaining partial steps=%v/%d", err, short.Short)
	}
	if v, dirty, _ := d.Version(); v != latestVersion || dirty {
		t.Fatalf("partial progress=%d/%v", v, dirty)
	}
	if err := m.Steps(-99); !errors.As(err, &short) || short.Short != 94 {
		t.Fatalf("down partial steps=%v/%d", err, short.Short)
	}
	if err := m.Migrate(3); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing target=%v", err)
	}
	if v, _, _ := d.Version(); v != -1 {
		t.Fatalf("absent target advanced version=%d", v)
	}
}

func TestUpstreamDirtyRecoveryAndNilMigration(t *testing.T) {
	files := fstest.MapFS{"1_first.up.sql": {Data: []byte("good")}, "1_first.down.sql": {Data: []byte("undo")}, "2_fail.up.sql": {Data: []byte("bad")}, "2_fail.down.sql": {Data: []byte("undo")}}
	d := &engineStore{version: -1, fail: "bad"}
	m := newEngine(t, files, d)
	if err := m.Up(); err == nil {
		t.Fatal("expected migration failure")
	}
	if v, dirty, _ := d.Version(); v != 2 || !dirty {
		t.Fatalf("failure did not retain dirty=%d/%v", v, dirty)
	}
	for _, run := range []func() error{m.Up, m.Down, func() error { return m.Steps(1) }, func() error { return m.Migrate(1) }} {
		var dirty migrate.ErrDirty
		if err := run(); !errors.As(err, &dirty) || dirty.Version != 2 {
			t.Fatalf("dirty guard=%v", err)
		}
	}
	if err := m.Force(1); err != nil {
		t.Fatal(err)
	}
	d.fail = ""
	if err := m.Up(); err != nil {
		t.Fatal(err)
	}
	if v, dirty, _ := d.Version(); v != 2 || dirty {
		t.Fatalf("recovery=%d/%v", v, dirty)
	}
	nilBody, err := migrate.NewMigration(nil, "nil-body", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Run(nilBody); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Version(); !errors.Is(err, migrate.ErrNilVersion) {
		t.Fatalf("nil migration=%v", err)
	}
}

func TestUpstreamTimeoutWaitsForObservedNativeLock(t *testing.T) {
	d := &engineStore{version: -1, lockDelay: 50 * time.Millisecond}
	observed := &observedLock{Driver: d, finished: make(chan error, 1)}
	src, err := iofs.New(fstest.MapFS{"1_noop.up.sql": {Data: []byte("ok")}}, ".")
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "contract", observed)
	if err != nil {
		t.Fatal(err)
	}
	m.LockTimeout = time.Millisecond
	if err := m.Up(); !errors.Is(err, migrate.ErrLockTimeout) {
		t.Fatalf("upstream timeout=%v", err)
	}
	select {
	case err := <-observed.finished:
		if !isTimeout(err) {
			t.Fatalf("native outcome=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("native lock outlived bound")
	}
	if a, b := m.Close(); a != nil || b != nil {
		t.Fatalf("close=%v/%v", a, b)
	}
}
