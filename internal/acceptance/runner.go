package acceptance

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	_ "github.com/golang-migrate/migrate/v4/source/file" // Register file sources for constructor coverage.
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

type suite struct {
	ctx      context.Context
	c        *connection
	profile  profile
	target   Target
	all      []Target
	report   TargetReport
	emit     func(TargetReport)
	d        database.Driver
	m        *migrate.Migrate
	tempDirs []string
	unsafe   bool
	// redactions holds every configured target's resolved DSN, captured once,
	// so recorded error details never repeat environment lookups.
	redactions []string
}

// Run executes a selected target. It never reads integration-test environment URLs.
func Run(ctx context.Context, target Target, all []Target, emit func(TargetReport)) TargetReport {
	s := &suite{ctx: ctx, target: target, all: all, emit: emit, report: TargetReport{Name: target.Name, Profile: target.Driver + "-" + target.Mode}}
	s.resolveRedactions()
	var err error
	s.profile, err = profileFor(target.Driver, target.Mode)
	if err != nil {
		s.record("configuration", Fail, err, 0)
		return s.report
	}
	dsn, err := target.resolveDSN()
	if err != nil {
		s.record("preflight", Skipped, err, 0)
		s.blockRemaining()
		return s.report
	}
	start := time.Now()
	s.c, err = connect(ctx, target, dsn)
	if err != nil {
		s.record("preflight", Fail, fmt.Errorf("%s", redactConnection(err.Error(), dsn)), time.Since(start))
		s.blockRemaining()
		return s.report
	}
	s.report.Server = s.c.info
	s.report.Server.Fingerprint = s.c.server + "/" + s.c.tenant
	s.run("preflight", nil, func() error { return s.requireEmpty() })
	if s.status("preflight") != Pass {
		s.unsafe = true
	}
	s.workflows()
	s.driverCases()
	s.typeCases()
	s.objectCases()
	s.crossSchema()
	closeErr := s.c.close()
	s.record("connection.close", statusFor(closeErr), closeErr, 0)
	s.report.Finished = true
	return s.report
}

func statusFor(err error) Status {
	if err != nil {
		return Fail
	}
	return Pass
}

func (s *suite) resolveRedactions() {
	for _, t := range s.all {
		if dsn, err := t.resolveDSN(); err == nil {
			s.redactions = append(s.redactions, dsn)
		}
	}
}

func (s *suite) record(id string, status Status, err error, duration time.Duration) {
	result := Result{ID: id, Status: status, Duration: duration}
	if err != nil {
		result.Detail = err.Error()
		if s.c != nil {
			result.Detail = redactConnection(result.Detail, s.c.dsn)
		}
		for _, dsn := range s.redactions {
			result.Detail = redactConnection(result.Detail, dsn)
		}
	}
	s.report.Results = append(s.report.Results, result)
	if s.emit != nil {
		s.emit(s.report)
	}
}

func (s *suite) status(id string) Status {
	for _, r := range s.report.Results {
		if r.ID == id {
			return r.Status
		}
	}
	return Blocked
}

func (s *suite) run(id string, dependencies []string, fn func() error) {
	for _, dep := range dependencies {
		if s.status(dep) != Pass {
			s.record(id, Blocked, fmt.Errorf("dependency %s did not pass", dep), 0)
			return
		}
	}
	if err := s.ctx.Err(); err != nil {
		s.record(id, Blocked, err, 0)
		return
	}
	start := time.Now()
	err := fn()
	s.record(id, statusFor(err), err, time.Since(start))
}

func (s *suite) isolated(id string, fn func() error) {
	if s.unsafe {
		s.record(id, Blocked, fmt.Errorf("schema preflight/cleanup did not succeed"), 0)
		return
	}
	s.run(id, nil, func() error {
		if err := s.requireEmpty(); err != nil {
			s.unsafe = true
			return err
		}
		return fn()
	})
	start := time.Now()
	err := s.cleanup()
	s.record(id+".cleanup", statusFor(err), err, time.Since(start))
	s.unsafe = err != nil || s.unsafe
}

func (s *suite) requireEmpty() error {
	if err := s.c.checkIdentity(s.ctx); err != nil {
		return err
	}
	objects, err := s.c.inventory(s.ctx)
	if err != nil {
		return err
	}
	if len(objects) != 0 {
		return fmt.Errorf("disposable schema is not empty: %v", objects)
	}
	return nil
}

var ownedObjects = map[string]bool{
	"mp_history": true, "mp_items": true, "mp_child": true, "mp_view": true, "mp_seq": true, "mp_type": true,
	"mp_native_type": true, "mp_native_enum": true, "mp_native_domain": true, "mp_probe": true, "mp_fn": true,
	"mp_proc": true, "mp_trigger": true, "mp_pkg": true, "mp_synonym": true, "mp_mv": true, "mp_sentinel": true,
	"mp_external": true, "mp_external_child": true, "mp_external_view": true, "mp_cycle_a": true, "mp_cycle_b": true,
}

func checkOwned(objects []object) error {
	for _, o := range objects {
		if !ownedObjects[strings.ToLower(o.Name)] {
			return fmt.Errorf("refuse destructive operation: unexpected %s %s", o.Kind, o.Name)
		}
	}
	return nil
}

func (s *suite) closeMigrator() error {
	var err error
	if s.m != nil {
		a, b := s.m.Close()
		err = errors.Join(a, b)
		s.m = nil
		s.d = nil
	} else if s.d != nil {
		err = s.d.Close()
		s.d = nil
	}
	for _, dir := range s.tempDirs {
		err = errors.Join(err, os.RemoveAll(dir))
	}
	s.tempDirs = nil
	return err
}

func (s *suite) cleanup() error {
	closeErr := s.closeMigrator()
	ctx, cancel := context.WithTimeout(context.Background(), s.target.statementTimeout())
	defer cancel()
	if err := s.c.checkIdentity(ctx); err != nil {
		return errors.Join(closeErr, err)
	}
	objects, err := s.c.inventory(ctx)
	if err != nil {
		return errors.Join(closeErr, err)
	}
	if err = checkOwned(objects); err != nil {
		return errors.Join(closeErr, err)
	}
	if len(objects) == 0 {
		return closeErr
	}
	d, err := s.c.driver("mp_history", s.target.statementTimeout())
	if err != nil {
		return errors.Join(closeErr, err)
	}
	// Drop is safe here only because preflight proved empty and every current root is owned.
	dropErr := d.Drop()
	driverCloseErr := d.Close()
	remaining, inventoryErr := s.c.inventory(ctx)
	if len(remaining) != 0 {
		inventoryErr = errors.Join(inventoryErr, fmt.Errorf("owned objects remain: %v", remaining))
	}
	return errors.Join(closeErr, dropErr, driverCloseErr, inventoryErr)
}

func (s *suite) open(fsys fs.FS, constructor string) error {
	if s.m != nil || s.d != nil {
		return fmt.Errorf("close previous migrator before opening another")
	}
	raw, err := s.c.migrationURL("mp_history", s.target.statementTimeout())
	if err != nil {
		return err
	}
	if constructor == constructorURL || constructor == "database" {
		return s.openFile(fsys, constructor, raw)
	}
	source, err := iofs.New(fsys, ".")
	if err != nil {
		return err
	}
	switch constructor {
	case "source":
		s.m, err = migrate.NewWithSourceInstance("iofs", source, raw)
	case "instance":
		s.d, err = s.c.driver("mp_history", s.target.statementTimeout())
		if err == nil {
			s.m, err = migrate.NewWithInstance("iofs", source, s.target.Driver, s.d)
		}
	default:
		err = fmt.Errorf("unknown constructor")
	}
	if err != nil {
		return errors.Join(err, source.Close(), s.closeMigrator())
	}
	s.m.LockTimeout = 2 * time.Second
	return nil
}

func (s *suite) openFile(fsys fs.FS, constructor, raw string) error {
	sourceURL, err := s.fileSource(fsys)
	if err != nil {
		return err
	}
	if constructor == constructorURL {
		s.m, err = migrate.New(sourceURL, raw)
	} else {
		s.d, err = s.c.driver("mp_history", s.target.statementTimeout())
		if err == nil {
			s.m, err = migrate.NewWithDatabaseInstance(sourceURL, s.target.Driver, s.d)
		}
	}
	if err != nil {
		return errors.Join(err, s.closeMigrator())
	}
	s.m.LockTimeout = 2 * time.Second
	return nil
}

func (s *suite) fileSource(fsys fs.FS) (string, error) {
	dir, err := os.MkdirTemp("", "migrate-regression-")
	if err != nil {
		return "", err
	}
	s.tempDirs = append(s.tempDirs, dir)
	err = fs.WalkDir(fsys, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		data, e := fs.ReadFile(fsys, path)
		if e != nil {
			return e
		}
		return os.WriteFile(filepath.Join(dir, filepath.Base(path)), data, 0600)
	})
	if err != nil {
		return "", err
	}
	// Opaque preserves a Windows drive letter without creating a leading slash.
	return (&url.URL{Scheme: "file", Opaque: filepath.ToSlash(dir)}).String(), nil
}

func (s *suite) exec(script string) error {
	if s.d == nil {
		var err error
		s.d, err = s.c.driver("mp_history", s.target.statementTimeout())
		if err != nil {
			return err
		}
	}
	return s.d.Run(strings.NewReader(script))
}

func (s *suite) dropTable(name string) string {
	statement := "DROP TABLE " + name
	if s.target.Driver == driverOracle {
		statement += " PURGE"
	}
	return statement
}

func (s *suite) scalar(query string) (string, error) {
	ctx, cancel := context.WithTimeout(s.ctx, s.target.statementTimeout())
	defer cancel()
	var value any
	if err := s.c.conn.QueryRowContext(ctx, query).Scan(&value); err != nil {
		return "", err
	}
	switch v := value.(type) {
	case nil:
		return nullValue, nil
	case []byte:
		return string(v), nil
	case string:
		return v, nil
	case time.Time:
		return v.Format(time.RFC3339Nano), nil
	default:
		return fmt.Sprint(v), nil
	}
}

func (s *suite) want(query, want string) error {
	got, err := s.scalar(query)
	if err != nil {
		return fmt.Errorf("observe %s: %w", query, err)
	}
	if got != want {
		return fmt.Errorf("observe %s: got %q, want %q", query, got, want)
	}
	return nil
}

func (s *suite) version(want int, dirty bool) error {
	if want == database.NilVersion {
		_, _, err := s.m.Version()
		if !errors.Is(err, migrate.ErrNilVersion) {
			return fmt.Errorf("empty version = %v", err)
		}
	} else {
		v, d, err := s.m.Version()
		if err != nil || int(v) != want || d != dirty {
			return fmt.Errorf("version=%d dirty=%v err=%v; want %d/%v", v, d, err, want, dirty)
		}
	}
	observer, err := s.c.driver("mp_history", s.target.statementTimeout())
	if err != nil {
		return err
	}
	v, d, readErr := observer.Version()
	closeErr := observer.Close()
	if readErr != nil || v != want || d != dirty {
		return errors.Join(fmt.Errorf("persisted version=%d dirty=%v err=%v; want %d/%v", v, d, readErr, want, dirty), closeErr)
	}
	return closeErr
}

func expectError(err error, class string) error {
	if err == nil {
		return fmt.Errorf("expected %s error, operation succeeded", class)
	}
	text := strings.ToLower(err.Error())
	matches := map[string][]string{
		"missing-object": {"ora-00942", "does not exist", "doesn't exist", "unknown table", "invalid table", "无效的表", "不存在"},
		errorUnique:      {"ora-00001", "23505", "duplicate entry", "duplicate key", "unique constraint", "唯一"},
		errorNull:        {"ora-01400", "23502", "cannot be null", "not-null constraint", "非空"},
		errorForeignKey:  {"ora-02291", "23503", "foreign key constraint", "外键", "引用约束"},
		errorCheck:       {"ora-02290", "23514", "check constraint", "检查约束", "check约束"},
		"object-exists":  {"error -2116:", "error -2124:"},
		"overflow":       {"ora-01438", "22003", "out of range", "numeric field overflow", "超出", "精度", "error -6149:"},
		"syntax":         {"ora-009", "42601", "syntax", "语法"},
	}
	for _, token := range matches[class] {
		if strings.Contains(text, token) {
			return nil
		}
	}
	return fmt.Errorf("expected %s error, got %w", class, err)
}

func redactConnection(message, dsn string) string {
	secrets := []string{dsn}
	if strings.HasPrefix(dsn, "dm://") {
		base := dsn
		if i := strings.LastIndex(base, "?"); i >= 0 && strings.Contains(base[:i], "@") {
			base = base[:i]
		}
		if at := strings.LastIndex(base, "@"); at > 5 {
			if colon := strings.Index(base[5:at], ":"); colon >= 0 {
				secrets = append(secrets, base[6+colon:at])
			}
		}
	} else if u, err := url.Parse(dsn); err == nil {
		if u.User != nil {
			if p, ok := u.User.Password(); ok {
				secrets = append(secrets, p, url.QueryEscape(p), url.PathEscape(p))
			}
		}
		if q, e := url.ParseQuery(u.RawQuery); e == nil {
			for key, values := range q {
				lower := strings.ToLower(key)
				if strings.Contains(lower, "pass") || strings.Contains(lower, "secret") || strings.Contains(lower, "token") {
					for _, v := range values {
						secrets = append(secrets, v, url.QueryEscape(v))
					}
				}
			}
		}
	}
	return redact(message, secrets...)
}
