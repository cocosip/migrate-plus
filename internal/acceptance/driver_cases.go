package acceptance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	obDriver "github.com/cocosip/migrate-plus/database/oceanbase"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
)

const typeSmallInt = "SMALLINT"

var driverNames = []string{caseDriverVersionRoundtrip, caseDriverHistoryCorrupt, "driver.history-invalid-shape", "driver.history-temporary", caseDriverLockLifecycle, caseDriverLockDdlContention, "driver.initialization-lock-timeout", "driver.statement-timeout", caseDriverClosedSession, caseDriverScriptFraming, caseDriverScriptRejections, "driver.procedure", caseDriverTransactions, caseDriverDropDependencies, caseDriverDropCycle, "driver.generated", "driver.constraints", "driver.integer-overflow", "driver.numeric-narrowing", "driver.literal-bytes"}

func (s *suite) operationContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(s.ctx, s.target.statementTimeout())
}

func (s *suite) nativeExec(sql string) error {
	ctx, cancel := s.operationContext()
	defer cancel()
	_, err := s.c.conn.ExecContext(ctx, sql)
	return err
}

func (s *suite) driverCases() {
	cases := []struct {
		id string
		fn func() error
	}{
		{caseDriverVersionRoundtrip, s.versionRoundtrip}, {caseDriverHistoryCorrupt, s.historyCorrupt},
		{"driver.history-invalid-shape", s.historyShape}, {"driver.history-temporary", s.historyTemporary},
		{caseDriverLockLifecycle, s.lockLifecycle}, {caseDriverLockDdlContention, s.lockContention},
		{"driver.initialization-lock-timeout", s.initializationTimeout}, {"driver.statement-timeout", s.statementTimeout},
		{caseDriverClosedSession, s.closedSession}, {caseDriverScriptFraming, s.scriptFraming},
		{caseDriverScriptRejections, s.scriptRejections}, {"driver.procedure", s.procedure},
		{caseDriverTransactions, s.transactions}, {caseDriverDropDependencies, s.dropDependencies},
		{caseDriverDropCycle, s.dropCycle}, {"driver.generated", s.generated}, {"driver.constraints", s.constraints},
		{"driver.integer-overflow", s.integerOverflow},
		{"driver.numeric-narrowing", s.numericNarrowing}, {"driver.literal-bytes", s.literalBytes},
	}
	for _, c := range cases {
		s.isolated(c.id, c.fn)
	}
	for _, ddl := range s.historyTypes() {
		s.isolated("history."+typeID(ddl), func() error { return s.historyLegacy(ddl) })
	}
	s.record("fault.external-disconnect", Skipped, fmt.Errorf("external network disruption/server KILL needs an explicitly supplied fault facility; close/timeout cases execute separately"), 0)
}

func (s *suite) ensureDriver() error {
	if s.d != nil {
		return nil
	}
	var err error
	s.d, err = s.c.driver("mp_history", s.target.statementTimeout())
	return err
}

func (s *suite) versionRoundtrip() (result error) {
	if err := s.ensureDriver(); err != nil {
		return err
	}
	observer, err := s.c.driver("mp_history", s.target.statementTimeout())
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, observer.Close()) }()
	for _, step := range []struct {
		v     int
		dirty bool
	}{{-1, false}, {-1, true}, {0, true}, {0, false}, {7, true}, {7, true}, {7, false}, {latestVersion, false}, {int(^uint(0) >> 1), false}, {-1, false}} {
		if err := s.d.SetVersion(step.v, step.dirty); err != nil {
			return err
		}
		v, d, e := observer.Version()
		if e != nil || v != step.v || d != step.dirty {
			return fmt.Errorf("SetVersion(%d,%v) observer=%d/%v/%v", step.v, step.dirty, v, d, e)
		}
	}
	return expectDiagnostic(s.d.SetVersion(-2, false), s.target.Driver+": version must be at least -1")
}

func (s *suite) historyTypes() []string {
	switch s.profile.Family {
	case driverOracle:
		return []string{"NUMBER(1)", typeNumber19}
	case driverDM:
		return []string{"BIT", typeSmallInt, typeInteger, typeBigInt, "DECIMAL(19,0)"}
	default:
		return []string{typeSmallInt, typeInteger, typeBigInt}
	}
}

func (s *suite) historyLegacy(ddl string) error {
	name := s.c.qualified("mp_history")
	if err := s.createLegacyHistory(ddl); err != nil {
		return err
	}
	if err := s.nativeExec("INSERT INTO " + name + " VALUES(7,1)"); err != nil {
		return err
	}
	if err := s.ensureDriver(); err != nil {
		return err
	}
	v, d, err := s.d.Version()
	if err != nil || v != 7 || !d {
		return fmt.Errorf("legacy history not preserved: %d/%v/%v", v, d, err)
	}
	if err := s.d.SetVersion(latestVersion, false); err != nil {
		return err
	}
	if err := s.closeMigrator(); err != nil {
		return err
	}
	if err := s.ensureDriver(); err != nil {
		return err
	}
	v, d, err = s.d.Version()
	if err != nil || v != latestVersion || d {
		return fmt.Errorf("legacy history reopen: %d/%v/%v", v, d, err)
	}
	return nil
}

func (s *suite) historyCorrupt() error {
	dirtyType := typeInteger
	if s.profile.Family == driverOracle {
		dirtyType = typeNumber19
	}
	if err := s.createLegacyHistory(dirtyType); err != nil {
		return err
	}
	if err := s.ensureDriver(); err != nil {
		return err
	}
	name := s.c.qualified("mp_history")
	for _, values := range []string{"(7,2)", "(-2,0)", "(7,0),(8,0)"} {
		if err := s.nativeExec("DELETE FROM " + name); err != nil {
			return err
		}
		// One INSERT per row is valid even on Oracle releases without multi-row VALUES.
		for _, row := range strings.Split(strings.TrimSuffix(strings.TrimPrefix(values, "("), ")"), "),(") {
			if err := s.nativeExec("INSERT INTO " + name + " VALUES(" + row + ")"); err != nil {
				return err
			}
		}
		_, _, err := s.d.Version()
		expected := s.target.Driver + ": corrupt or multiple migration state rows"
		if values == "(7,2)" && s.target.Driver == driverDM {
			expected = "dm: invalid DIRTY value 2 ("
		}
		if err := expectDiagnostic(err, expected); err != nil {
			return fmt.Errorf("corrupt history %s: %w", values, err)
		}
	}
	return s.nativeExec("DELETE FROM " + name)
}

func (s *suite) createLegacyHistory(dirtyType string) error {
	versionType := typeBigInt
	if s.profile.Family == driverOracle {
		versionType = typeNumber19
	}
	return s.nativeExec("CREATE TABLE " + s.c.qualified("mp_history") + "(version " + versionType + " NOT NULL PRIMARY KEY, dirty " + dirtyType + " NOT NULL)")
}

func expectDiagnostic(err error, diagnostic string) error {
	if err == nil {
		return fmt.Errorf("expected %q rejection, operation succeeded", diagnostic)
	}
	if !strings.Contains(err.Error(), diagnostic) {
		return fmt.Errorf("expected %q rejection, got %w", diagnostic, err)
	}
	return nil
}

func (s *suite) historyShape() error {
	name := s.c.qualified("mp_history")
	if err := s.nativeExec("CREATE TABLE " + name + "(version VARCHAR(40),dirty INTEGER)"); err != nil {
		return err
	}
	d, err := s.c.driver("mp_history", s.target.statementTimeout())
	if err == nil {
		return errors.Join(fmt.Errorf("accepted non-integral history version column"), d.Close())
	}
	expected := s.target.Driver + ": migration state requires non-null integral version and dirty columns"
	switch s.profile.Family {
	case driverDM:
		expected = "dm: migration state columns must be numeric integers"
	case modePG:
		expected = s.target.Driver + ": state requires a BIGINT primary version and non-null integer dirty column"
	}
	if err := expectDiagnostic(err, expected); err != nil {
		return err
	}
	if err := s.want("SELECT COUNT(*) FROM "+name, "0"); err != nil {
		return err
	}
	// Remove the exact invalid fixture; a new driver must not repair it automatically.
	return s.nativeExec(s.dropTable(name))
}

func (s *suite) historyTemporary() error {
	name := s.c.qualified("mp_history")
	ddl := "CREATE GLOBAL TEMPORARY TABLE " + name + "(version DECIMAL(19,0) NOT NULL,dirty INTEGER NOT NULL) ON COMMIT PRESERVE ROWS"
	switch s.profile.Family {
	case modeMySQL:
		return s.temporaryMySQL()
	case modePG:
		ddl = "CREATE UNLOGGED TABLE " + name + "(version BIGINT NOT NULL,dirty INTEGER NOT NULL)"
	}
	if err := s.nativeExec(ddl); err != nil {
		return err
	}
	d, err := s.c.driver("mp_history", s.target.statementTimeout())
	if err == nil {
		return errors.Join(fmt.Errorf("accepted temporary migration history"), d.Close())
	}
	expected := "ORA-00955"
	if s.target.Driver == driverOceanBase {
		expected = "server error 955 ("
	}
	switch s.profile.Family {
	case driverDM:
		if err := expectError(err, "object-exists"); err != nil {
			return err
		}
		return s.nativeExec(s.dropTable(name))
	case modePG:
		expected = s.target.Driver + ": migration state must be a permanent ordinary table"
	}
	if err := expectDiagnostic(err, expected); err != nil {
		return err
	}
	return s.nativeExec(s.dropTable(name))
}

func (s *suite) temporaryMySQL() (result error) {
	u, err := url.Parse(s.c.dsn)
	if err != nil {
		return err
	}
	options := u.Query()
	// A full pool reset replaces the physical connection and destroys this fixture.
	options.Set("resetMode", "managed")
	u.RawQuery = options.Encode()
	db, err := nativePool(s.target, u.String(), s.target.statementTimeout())
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, db.Close()) }()
	ctx, cancel := s.operationContext()
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	probe := &connection{conn: conn, target: s.target, dsn: s.c.dsn, info: s.c.info, server: s.c.server, tenant: s.c.tenant}
	err = probe.checkIdentity(ctx)
	if err == nil {
		_, err = conn.ExecContext(ctx, "CREATE TEMPORARY TABLE "+s.c.qualified("mp_history")+"(version BIGINT NOT NULL PRIMARY KEY,dirty INTEGER NOT NULL)")
	}
	if err = errors.Join(err, conn.Close()); err != nil {
		return err
	}
	// nativePool retains exactly one idle session, so WithInstance sees the temporary table.
	d, err := obDriver.WithInstance(db, &obDriver.Config{MigrationsTable: "mp_history", StatementTimeout: s.target.statementTimeout()})
	if err == nil {
		return errors.Join(fmt.Errorf("accepted session-temporary migration history"), d.Close())
	}
	return expectDiagnostic(err, "server error 1050 (")
}

func (s *suite) lockLifecycle() error {
	if err := s.ensureDriver(); err != nil {
		return err
	}
	if err := s.d.Lock(); err != nil {
		return err
	}
	if err := s.d.Lock(); !errors.Is(err, database.ErrLocked) {
		return fmt.Errorf("duplicate Lock=%v", err)
	}
	if err := s.d.Unlock(); err != nil {
		return err
	}
	if err := s.d.Unlock(); !errors.Is(err, database.ErrNotLocked) {
		return fmt.Errorf("unowned Unlock=%v", err)
	}
	if err := s.d.Lock(); err != nil {
		return err
	}
	if err := s.d.Close(); err != nil {
		return err
	}
	s.d = nil
	if err := s.ensureDriver(); err != nil {
		return err
	}
	if err := s.d.Lock(); err != nil {
		return err
	}
	return s.d.Unlock()
}

func (s *suite) lockContention() (result error) {
	if err := s.ensureDriver(); err != nil {
		return err
	}
	other, err := s.c.driver("mp_history", 300*time.Millisecond)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, other.Close()) }()
	if err := s.d.Lock(); err != nil {
		return err
	}
	if err := s.exec("CREATE TABLE mp_probe(id INTEGER)"); err != nil {
		return err
	}
	start := time.Now()
	err = other.Lock()
	if err == nil {
		return errors.Join(fmt.Errorf("DDL released migration lock"), other.Unlock())
	}
	if !isTimeout(err) {
		return fmt.Errorf("expected bounded contention: %w", err)
	}
	if time.Since(start) > 3*time.Second {
		return fmt.Errorf("driver lock wait exceeded bound")
	}
	if err := s.d.Unlock(); err != nil {
		return err
	}
	return s.want("SELECT COUNT(*) FROM mp_probe", "0")
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, migrate.ErrLockTimeout) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "deadline exceeded") || strings.Contains(text, "timeout") || strings.Contains(text, "timed out")
}

func (s *suite) initializationTimeout() (result error) {
	if err := s.ensureDriver(); err != nil {
		return err
	}
	if err := s.d.Lock(); err != nil {
		return err
	}
	start := time.Now()
	other, err := s.c.driver("mp_history", 300*time.Millisecond)
	if err == nil {
		return errors.Join(fmt.Errorf("initialization ignored held metadata lock"), other.Close())
	}
	if !isTimeout(err) {
		return fmt.Errorf("initialization contention=%w", err)
	}
	if time.Since(start) > 3*time.Second {
		return fmt.Errorf("initialization wait exceeded bound")
	}
	return s.d.Unlock()
}

func (s *suite) statementTimeout() error {
	d, err := s.c.driver("mp_history", 100*time.Millisecond)
	if err != nil {
		return err
	}
	s.d = d
	if err = d.Lock(); err != nil {
		return err
	}
	script := "SELECT pg_catalog.pg_sleep(2)"
	switch s.profile.Family {
	case driverOracle, driverDM:
		script = "BEGIN DBMS_LOCK.SLEEP(2); END;\n/"
	case modeMySQL:
		script = "SELECT SLEEP(2)"
	}
	start := time.Now()
	err = d.Run(strings.NewReader(script))
	if err == nil || !isTimeout(err) {
		return fmt.Errorf("slow statement should time out: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		return fmt.Errorf("statement timeout exceeded bound")
	}
	if err := d.Run(strings.NewReader("CREATE TABLE mp_probe(id INTEGER)")); !errors.Is(err, sql.ErrConnDone) {
		return fmt.Errorf("timed-out session should be discarded: %v", err)
	}
	if err := d.Close(); err != nil {
		return err
	}
	s.d = nil
	if err := s.ensureDriver(); err != nil {
		return err
	}
	if err := s.d.Lock(); err != nil {
		return err
	}
	return s.d.Unlock()
}

func (s *suite) closedSession() error {
	if err := s.ensureDriver(); err != nil {
		return err
	}
	d := s.d
	if err := d.Close(); err != nil {
		return err
	}
	if err := d.Close(); err != nil {
		return err
	}
	s.d = nil
	if err := d.Run(strings.NewReader("CREATE TABLE mp_probe(id INTEGER)")); !errors.Is(err, sql.ErrConnDone) {
		return fmt.Errorf("closed session should reject Run with ErrConnDone: %v", err)
	}
	return s.ensureDriver()
}

func (s *suite) scriptFraming() error {
	script := "-- comment ;\nCREATE TABLE mp_probe(id INTEGER PRIMARY KEY,v VARCHAR(200)); /* ; */\nINSERT INTO mp_probe VALUES(1,'quoted '';value'); INSERT INTO mp_probe VALUES(2,'中文');"
	if err := s.exec(script); err != nil {
		return err
	}
	if err := s.want("SELECT v FROM mp_probe WHERE id=1", "quoted ';value"); err != nil {
		return err
	}
	if err := s.exec("-- comment-only ;\n/* empty ; */"); err != nil {
		return err
	}
	return s.want("SELECT COUNT(*) FROM mp_probe", "2")
}

func (s *suite) scriptRejections() error {
	if err := s.ensureDriver(); err != nil {
		return err
	}
	quoteError := s.target.Driver + ": unclosed quoted value"
	commentError := s.target.Driver + ": unclosed comment"
	commandError := s.target.Driver + ": unsupported client command or COPY framing"
	switch s.profile.Family {
	case driverDM:
		commentError = "dm: unclosed or nested comment"
	case modePG:
		quoteError = s.target.Driver + ": unterminated SQL quote"
		commentError = s.target.Driver + ": unterminated SQL comment"
		commandError = s.target.Driver + ": COPY, client commands and unquoted procedural blocks are unsupported"
	}
	if s.target.Driver == driverOceanBase {
		commentError = "oceanbase: unclosed or nested SQL comment"
		commandError = "oceanbase: session, transaction and client commands are not supported in migration scripts"
	}
	scripts := []struct{ sql, diagnostic string }{
		{"CREATE TABLE mp_probe(id INTEGER); SELECT 'unterminated", quoteError},
		{"/* unterminated", commentError},
	}
	if s.profile.Family != driverOracle {
		scripts = append(scripts, struct{ sql, diagnostic string }{"CREATE TABLE mp_probe(id INTEGER); DELIMITER //", commandError}, struct{ sql, diagnostic string }{"CREATE TABLE mp_probe(id INTEGER); COPY mp_probe FROM STDIN;", commandError})
	}
	for _, script := range scripts {
		if err := expectDiagnostic(s.exec(script.sql), script.diagnostic); err != nil {
			return err
		}
		ctx, cancel := s.operationContext()
		objects, err := s.c.inventory(ctx)
		cancel()
		if err != nil {
			return err
		}
		for _, o := range objects {
			if strings.EqualFold(o.Name, "mp_probe") {
				return fmt.Errorf("SQL preflight executed preceding statement before rejection")
			}
		}
	}
	return nil
}

func (s *suite) procedure() error {
	if err := s.exec("CREATE TABLE mp_probe(id INTEGER)"); err != nil {
		return err
	}
	script := "CREATE FUNCTION mp_fn(integer) RETURNS integer AS $$ SELECT $1+1 $$ LANGUAGE SQL; INSERT INTO mp_probe VALUES(mp_fn(6));"
	switch s.profile.Family {
	case driverOracle, driverDM:
		script = "CREATE OR REPLACE PROCEDURE mp_proc AS BEGIN INSERT INTO mp_probe VALUES(7); END;\n/\nBEGIN mp_proc; END;\n/"
	case modeMySQL:
		script = "CREATE PROCEDURE mp_proc() BEGIN DECLARE v INT; SET v=7; INSERT INTO mp_probe VALUES(v); END; CALL mp_proc();"
	}
	if err := s.exec(script); err != nil {
		return err
	}
	return s.want("SELECT id FROM mp_probe", "7")
}

func (s *suite) transactions() error {
	if err := s.exec("CREATE TABLE mp_probe(id INTEGER)"); err != nil {
		return err
	}
	if s.profile.Family == modeMySQL || s.target.Driver == driverOceanBase {
		return expectDiagnostic(s.exec("COMMIT"), "oceanbase: session, transaction and client commands are not supported in migration scripts")
	}
	script := "BEGIN; INSERT INTO mp_probe VALUES(1); COMMIT; BEGIN; INSERT INTO mp_probe VALUES(2); ROLLBACK;"
	if s.profile.Family == driverOracle || s.profile.Family == driverDM {
		script = "INSERT INTO mp_probe VALUES(1); COMMIT; INSERT INTO mp_probe VALUES(2); ROLLBACK;"
	}
	if err := s.exec(script); err != nil {
		return err
	}
	// Oracle/DM autocommit commits standalone DML; verify their native contract explicitly.
	count := "1"
	if s.profile.Family == driverOracle || s.profile.Family == driverDM {
		count = "2"
	}
	if err := s.want("SELECT COUNT(*) FROM mp_probe", count); err != nil {
		return err
	}
	if s.profile.Family == modePG {
		if err := s.exec("BEGIN; INSERT INTO mp_probe VALUES(3);"); err == nil || !strings.Contains(err.Error(), "open transaction") {
			return fmt.Errorf("unfinished transaction=%v", err)
		}
		return s.want("SELECT COUNT(*) FROM mp_probe", "1")
	}
	return nil
}

func (s *suite) dropDependencies() error {
	if err := s.open(s.profile.FS, "instance"); err != nil {
		return err
	}
	if err := s.m.Up(); err != nil {
		return err
	}
	ctx, cancel := s.operationContext()
	objects, err := s.c.inventory(ctx)
	cancel()
	if err != nil {
		return err
	}
	if err := checkOwned(objects); err != nil {
		return err
	}
	if err := s.m.Drop(); err != nil {
		return err
	}
	if err := s.closeMigrator(); err != nil {
		return err
	}
	if err := s.requireEmpty(); err != nil {
		return err
	}
	if err := s.open(s.profile.FS, "instance"); err != nil {
		return err
	}
	return s.version(-1, false)
}

func (s *suite) dropCycle() error {
	if err := s.exec("CREATE TABLE mp_cycle_a(id INTEGER PRIMARY KEY,b INTEGER); CREATE TABLE mp_cycle_b(id INTEGER PRIMARY KEY,a INTEGER, CONSTRAINT mp_cycle_b_fk FOREIGN KEY(a) REFERENCES mp_cycle_a(id)); ALTER TABLE mp_cycle_a ADD CONSTRAINT mp_cycle_fk FOREIGN KEY(b) REFERENCES mp_cycle_b(id);"); err != nil {
		return err
	}
	if err := s.d.Drop(); err != nil {
		return err
	}
	if err := s.closeMigrator(); err != nil {
		return err
	}
	return s.requireEmpty()
}
