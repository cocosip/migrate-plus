package acceptance

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing/fstest"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/source"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

const latestVersion = 202610230001

var workflowNames = []string{"workflow.lock-timeout", caseConstructorURL, "constructor.source", "constructor.database", "constructor.instance", caseWorkflowStepsUp, caseWorkflowDownReapply, "workflow.step-boundaries", "workflow.missing-version", "workflow.force", "workflow.run", "workflow.nil-migration", caseWorkflowDirtyUp, "workflow.dirty-down", "workflow.prefetch-0", "workflow.prefetch-1", "workflow.prefetch-10", "workflow.stop-before", "workflow.stop-between", "source.validation", "source.read-failure", "source.close-failure"}

func (s *suite) workflows() {
	s.dataWorkflows()
	s.isolated("workflow.lock-timeout", s.engineLockTimeout)
	for _, constructor := range []string{constructorURL, "source", "database", "instance"} {
		s.isolated("constructor."+constructor, func() error {
			if err := s.open(s.profile.FS, constructor); err != nil {
				return err
			}
			if err := s.m.Up(); err != nil {
				return err
			}
			return s.latest()
		})
	}
	s.isolated(caseWorkflowStepsUp, s.stepsUp)
	s.isolated(caseWorkflowDownReapply, s.downReapply)
	s.isolated("workflow.step-boundaries", s.stepBoundaries)
	s.isolated("workflow.missing-version", func() error {
		if err := s.open(s.profile.FS, "instance"); err != nil {
			return err
		}
		if err := s.m.Migrate(3); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("missing target error: %v", err)
		}
		return s.version(-1, false)
	})
	s.isolated("workflow.force", s.force)
	s.isolated("workflow.run", s.explicitRun)
	s.isolated("workflow.nil-migration", s.nilMigration)
	s.isolated(caseWorkflowDirtyUp, func() error { return s.dirty(false) })
	s.isolated("workflow.dirty-down", func() error { return s.dirty(true) })
	for _, prefetch := range []uint{0, 1, 10} {
		s.isolated(fmt.Sprintf("workflow.prefetch-%d", prefetch), func() error {
			files, err := copyFixtures(s.profile.FS)
			if err != nil {
				return err
			}
			body := files["0_init.up.sql"]
			if body == nil {
				for name, file := range files {
					if strings.HasPrefix(name, "0_") && strings.HasSuffix(name, ".up.sql") {
						body = file
						break
					}
				}
			}
			if body == nil {
				return fmt.Errorf("missing version-zero fixture")
			}
			body.Data = append([]byte("/*"+strings.Repeat(" buffered; ", 12000)+"*/\n"), body.Data...)
			if err := s.open(files, "instance"); err != nil {
				return err
			}
			s.m.PrefetchMigrations = prefetch
			if err := s.m.Up(); err != nil {
				return err
			}
			return s.latest()
		})
	}
	s.isolated("workflow.stop-before", func() error {
		if err := s.open(s.profile.FS, "instance"); err != nil {
			return err
		}
		s.m.GracefulStop <- true
		if err := s.m.Up(); err != nil {
			return err
		}
		if err := s.version(-1, false); err != nil {
			return err
		}
		if err := s.closeMigrator(); err != nil {
			return err
		}
		if err := s.open(s.profile.FS, "instance"); err != nil {
			return err
		}
		if err := s.m.Up(); err != nil {
			return err
		}
		return s.latest()
	})
	s.isolated("workflow.stop-between", func() error {
		if err := s.open(s.profile.FS, "instance"); err != nil {
			return err
		}
		s.m.PrefetchMigrations = 0
		s.m.Log = &stopLogger{stop: s.m.GracefulStop}
		if err := s.m.Up(); err != nil {
			return err
		}
		if err := s.version(0, false); err != nil {
			return err
		}
		if err := s.want("SELECT name FROM mp_items WHERE id=1", "seed"); err != nil {
			return err
		}
		if err := s.closeMigrator(); err != nil {
			return err
		}
		if err := s.open(s.profile.FS, "instance"); err != nil {
			return err
		}
		if err := s.m.Up(); err != nil {
			return err
		}
		return s.latest()
	})
	s.isolated("source.validation", s.sourceValidation)
	s.isolated("source.read-failure", s.sourceReadFailure)
	s.isolated("source.close-failure", s.sourceCloseFailure)
}

type observedLock struct {
	database.Driver
	finished chan error
}

func (d *observedLock) Lock() error {
	err := d.Driver.Lock()
	d.finished <- err
	return err
}

func (s *suite) engineLockTimeout() (result error) {
	if err := s.open(s.profile.FS, "instance"); err != nil {
		return err
	}
	other, err := s.c.driver("mp_history", 300*time.Millisecond)
	if err != nil {
		return err
	}
	src, err := iofs.New(s.profile.FS, ".")
	if err != nil {
		return errors.Join(err, other.Close())
	}
	observed := &observedLock{Driver: other, finished: make(chan error, 1)}
	m, err := migrate.NewWithInstance("iofs", src, s.target.Driver, observed)
	if err != nil {
		return errors.Join(err, src.Close(), other.Close())
	}
	defer func() { a, b := m.Close(); result = errors.Join(result, a, b) }()
	if err := s.d.Lock(); err != nil {
		return err
	}
	locked := true
	defer func() {
		if locked {
			result = errors.Join(result, s.d.Unlock())
		}
	}()
	m.LockTimeout = 50 * time.Millisecond
	if err := m.Up(); !errors.Is(err, migrate.ErrLockTimeout) {
		return fmt.Errorf("upstream LockTimeout=%v", err)
	}
	select {
	case err := <-observed.finished:
		if !isTimeout(err) {
			return fmt.Errorf("contender lock outcome=%v", err)
		}
	case <-time.After(3 * time.Second):
		return fmt.Errorf("native lock did not settle after upstream timeout")
	}
	if err := s.d.Unlock(); err != nil {
		return err
	}
	locked = false
	return s.version(-1, false)
}

func (s *suite) latest() error {
	for query, want := range map[string]string{"SELECT COUNT(*) FROM mp_items": "2", "SELECT name FROM mp_items WHERE id=1": "updated", "SELECT name FROM mp_items WHERE id=2": "second", "SELECT note FROM mp_items WHERE id=1": "backfill", "SELECT COUNT(*) FROM mp_child": "1", "SELECT COUNT(*) FROM mp_view": "2"} {
		if err := s.want(query, want); err != nil {
			return err
		}
	}
	return s.version(latestVersion, false)
}

func (s *suite) stepsUp() error {
	if err := s.open(s.profile.FS, "instance"); err != nil {
		return err
	}
	if err := s.version(-1, false); err != nil {
		return err
	}
	for _, v := range []int{0, 1, 2, 4, latestVersion} {
		if err := s.m.Steps(1); err != nil {
			return err
		}
		if err := s.version(v, false); err != nil {
			return err
		}
		if err := s.want("SELECT COUNT(*) FROM mp_items", map[bool]string{true: "2", false: "1"}[v == latestVersion]); err != nil {
			return err
		}
		if v >= 2 {
			if err := s.want("SELECT note FROM mp_items WHERE id=1", "backfill"); err != nil {
				return err
			}
		}
	}
	for _, fn := range []func() error{s.m.Up, func() error { return s.m.Steps(0) }, func() error { return s.m.Migrate(latestVersion) }} {
		if err := fn(); !errors.Is(err, migrate.ErrNoChange) {
			return fmt.Errorf("repeat/no-op: %v", err)
		}
	}
	if err := s.closeMigrator(); err != nil {
		return err
	}
	if err := s.open(s.profile.FS, "instance"); err != nil {
		return err
	}
	return s.latest()
}

func (s *suite) downReapply() error {
	if err := s.open(s.profile.FS, "instance"); err != nil {
		return err
	}
	if err := s.m.Up(); err != nil {
		return err
	}
	if err := s.m.Steps(-1); err != nil {
		return err
	}
	if err := s.version(4, false); err != nil {
		return err
	}
	if err := s.want("SELECT COUNT(*) FROM mp_items", "1"); err != nil {
		return err
	}
	if err := s.m.Migrate(2); err != nil {
		return err
	}
	if err := s.version(2, false); err != nil {
		return err
	}
	if err := s.want("SELECT name FROM mp_items WHERE id=1", "seed"); err != nil {
		return err
	}
	if err := s.m.Migrate(0); err != nil {
		return err
	}
	if err := s.version(0, false); err != nil {
		return err
	}
	if err := s.m.Down(); err != nil {
		return err
	}
	if err := s.version(-1, false); err != nil {
		return err
	}
	if err := s.m.Down(); !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("repeat down = %v", err)
	}
	if err := s.m.Up(); err != nil {
		return err
	}
	return s.latest()
}

func (s *suite) stepBoundaries() error {
	if err := s.open(s.profile.FS, "instance"); err != nil {
		return err
	}
	var short migrate.ErrShortLimit
	if err := s.m.Steps(99); !errors.As(err, &short) || short.Short != 94 {
		return fmt.Errorf("positive short limit = %v (%d)", err, short.Short)
	}
	if err := s.latest(); err != nil {
		return err
	}
	if err := s.m.Steps(-99); !errors.As(err, &short) || short.Short != 94 {
		return fmt.Errorf("negative short limit = %v (%d)", err, short.Short)
	}
	if err := s.version(-1, false); err != nil {
		return err
	}
	if err := s.m.Steps(5); err != nil {
		return err
	}
	if err := s.latest(); err != nil {
		return err
	}
	if err := s.m.Steps(-5); err != nil {
		return err
	}
	return s.version(-1, false)
}

func (s *suite) force() error {
	if err := s.open(s.profile.FS, "instance"); err != nil {
		return err
	}
	if err := s.m.Steps(1); err != nil {
		return err
	}
	if err := s.m.Force(-2); !errors.Is(err, migrate.ErrInvalidVersion) {
		return fmt.Errorf("invalid force = %v", err)
	}
	for _, v := range []int{7, 0, -1, 0} {
		if err := s.m.Force(v); err != nil {
			return err
		}
		if err := s.version(v, false); err != nil {
			return err
		}
		if err := s.want("SELECT name FROM mp_items WHERE id=1", "seed"); err != nil {
			return err
		}
	}
	if err := s.m.Up(); err != nil {
		return err
	}
	return s.latest()
}

func (s *suite) explicitRun() error {
	if err := s.open(s.profile.FS, "instance"); err != nil {
		return err
	}
	if err := s.m.Run(); !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("empty Run=%v", err)
	}
	body := io.NopCloser(strings.NewReader("CREATE TABLE mp_probe(id INTEGER); INSERT INTO mp_probe VALUES(1);"))
	first, err := migrate.NewMigration(body, "explicit", 9, 9)
	if err != nil {
		return err
	}
	second, err := migrate.NewMigration(nil, "nil-body", 3, 3)
	if err != nil {
		return err
	}
	if err := s.m.Run(first, second); err != nil {
		return err
	}
	if err := s.version(3, false); err != nil {
		return err
	}
	if err := s.want("SELECT COUNT(*) FROM mp_probe", "1"); err != nil {
		return err
	}
	last, err := migrate.NewMigration(nil, "empty-version", 0, -1)
	if err != nil {
		return err
	}
	if err := s.m.Run(last); err != nil {
		return err
	}
	return s.version(-1, false)
}

func (s *suite) nilMigration() error {
	files := fstest.MapFS{"1_only.up.sql": {Data: []byte("-- empty migration\n")}, "2_only.down.sql": {Data: []byte("-- empty down\n")}}
	if err := s.open(files, "instance"); err != nil {
		return err
	}
	if err := s.m.Up(); err != nil {
		return err
	}
	if err := s.version(2, false); err != nil {
		return err
	}
	if err := s.m.Down(); err != nil {
		return err
	}
	return s.version(-1, false)
}

func copyFixtures(fsys fs.FS) (fstest.MapFS, error) {
	result := fstest.MapFS{}
	err := fs.WalkDir(fsys, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, e := fs.ReadFile(fsys, path)
		if e == nil {
			result[path] = &fstest.MapFile{Data: data}
		}
		return e
	})
	return result, err
}

func (s *suite) dirty(down bool) error {
	files, err := copyFixtures(s.profile.FS)
	if err != nil {
		return err
	}
	failSQL := "INSERT INTO mp_missing_object VALUES(1);"
	if down {
		for name, file := range files {
			if strings.HasPrefix(name, fmt.Sprint(latestVersion)+"_") && strings.HasSuffix(name, ".down.sql") {
				file.Data = []byte(failSQL)
			}
		}
	} else {
		files["202610230002_fail.up.sql"] = &fstest.MapFile{Data: []byte("UPDATE mp_items SET name='partial' WHERE id=1; " + failSQL)}
		files["202610230002_fail.down.sql"] = &fstest.MapFile{Data: []byte("-- repaired explicitly")}
	}
	if err := s.open(files, "instance"); err != nil {
		return err
	}
	failingVersion := latestVersion + 1
	if down {
		if err := s.m.Up(); err != nil {
			return err
		}
		failingVersion = 4
		err = s.m.Steps(-1)
	} else {
		err = s.m.Up()
	}
	if err := expectError(err, "missing-object"); err != nil {
		return err
	}
	if err := s.version(failingVersion, true); err != nil {
		return err
	}
	probe, err := migrate.NewMigration(nil, "dirty", 1, 1)
	if err != nil {
		return err
	}
	for _, fn := range []func() error{s.m.Up, s.m.Down, func() error { return s.m.Steps(1) }, func() error { return s.m.Migrate(0) }, func() error { return s.m.Run(probe) }} {
		var dirty migrate.ErrDirty
		if e := fn(); !errors.As(e, &dirty) || dirty.Version != failingVersion {
			return fmt.Errorf("dirty operation error=%v", e)
		}
	}
	// Explicitly restore only the synthetic failed fixture's data before Force.
	if err := s.exec("UPDATE mp_items SET name='updated' WHERE id=1"); err != nil {
		return err
	}
	if err := s.m.Force(latestVersion); err != nil {
		return err
	}
	if err := s.latest(); err != nil {
		return err
	}
	if err := s.closeMigrator(); err != nil {
		return err
	}
	if err := s.open(s.profile.FS, "instance"); err != nil {
		return err
	}
	if err := s.m.Down(); err != nil {
		return err
	}
	return s.version(-1, false)
}

type stopLogger struct {
	once sync.Once
	stop chan bool
}

func (l *stopLogger) Verbose() bool { return false }
func (l *stopLogger) Printf(format string, _ ...interface{}) {
	if format == "%v (%v)\n" {
		l.once.Do(func() { l.stop <- true })
	}
}

func (s *suite) sourceValidation() error {
	files := fstest.MapFS{"1_a.up.sql": {Data: []byte("--a")}, "1_b.up.sql": {Data: []byte("--b")}}
	_, err := iofs.New(files, ".")
	var duplicate source.ErrDuplicateMigration
	if !errors.As(err, &duplicate) {
		return fmt.Errorf("duplicate source=%v", err)
	}
	src, err := iofs.New(fstest.MapFS{"invalid.sql": {Data: []byte("bad")}}, ".")
	if err != nil {
		return err
	}
	_, firstErr := src.First()
	closeErr := src.Close()
	if !errors.Is(firstErr, os.ErrNotExist) {
		return errors.Join(fmt.Errorf("invalid filename should be ignored: %v", firstErr), closeErr)
	}
	if closeErr != nil {
		return closeErr
	}
	if _, err := source.Open("unknown-acceptance://none"); err == nil {
		return fmt.Errorf("unknown source accepted")
	}
	return nil
}

type brokenFS struct{ fs.FS }

func (f brokenFS) Open(name string) (fs.File, error) {
	if strings.HasSuffix(name, ".sql") {
		return nil, fmt.Errorf("acceptance-source-read-failure")
	}
	return f.FS.Open(name)
}

func (s *suite) sourceReadFailure() error {
	if err := s.open(brokenFS{s.profile.FS}, "instance"); err != nil {
		return err
	}
	if err := s.m.Up(); err == nil || !strings.Contains(err.Error(), "acceptance-source-read-failure") {
		return fmt.Errorf("source read failure=%v", err)
	}
	return s.version(-1, false)
}

type closeFailFS struct{ fs.FS }

func (f closeFailFS) Close() error { return fmt.Errorf("acceptance-source-close-failure") }

func (s *suite) sourceCloseFailure() error {
	if err := s.open(closeFailFS{s.profile.FS}, "instance"); err != nil {
		return err
	}
	srcErr, dbErr := s.m.Close()
	s.m = nil
	s.d = nil
	if srcErr == nil || !strings.Contains(srcErr.Error(), "acceptance-source-close-failure") || dbErr != nil {
		return fmt.Errorf("close errors source=%v database=%v", srcErr, dbErr)
	}
	return nil
}
