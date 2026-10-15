package oracle

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/golang-migrate/migrate/v4/database"
	go_ora "github.com/sijms/go-ora/v2"
)

func init() {
	database.Register("oracle", &Oracle{})
}

// Oracle holds a dedicated session so DDL cannot release the migration lock.
type Oracle struct {
	db       *sql.DB
	conn     *sql.Conn
	config   Config
	schema   string
	lockID   int64
	isLocked atomic.Bool
}

var _ database.Driver = (*Oracle)(nil)

// WithInstance takes ownership of instance on success; Close closes its pool.
// On failure the pool remains caller-owned and the reserved session is discarded.
func WithInstance(instance *sql.DB, config *Config) (_ database.Driver, err error) {
	if instance == nil {
		return nil, fmt.Errorf("oracle: nil database instance")
	}
	normalized, err := prepareConfig(config, capabilities{identifierLimit: 128})
	if err != nil {
		return nil, err
	}
	o := &Oracle{db: instance, config: normalized}
	ctx, cancel := o.operationContext()
	defer cancel()
	o.conn, err = instance.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			// Never return a session with a possibly unreleased lock to the pool.
			_ = o.conn.Raw(func(any) error { return sqldriver.ErrBadConn })
		}
	}()
	if err = o.initialize(); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *Oracle) initialize() error {
	var server, compatible string
	if err := o.exec(`BEGIN DBMS_UTILITY.DB_VERSION(:1, :2); END;`,
		go_ora.Out{Dest: &server, Size: 64}, go_ora.Out{Dest: &compatible, Size: 64}); err != nil {
		return err
	}
	caps, err := capabilitiesFor(server, compatible)
	if err != nil {
		return err
	}
	o.config, err = prepareConfig(&o.config, caps)
	if err != nil {
		return err
	}
	ctx, cancel := o.operationContext()
	defer cancel()
	query := `SELECT SYS_CONTEXT('USERENV', 'CURRENT_SCHEMA') FROM dual`
	if err := o.conn.QueryRowContext(ctx, query).Scan(&o.schema); err != nil {
		return queryError(query, err)
	}
	if o.schema == "" {
		return fmt.Errorf("oracle: empty current schema")
	}
	lockID, err := database.GenerateAdvisoryLockId(o.schema, o.config.MigrationsTable)
	if err != nil {
		return err
	}
	numericID, err := strconv.ParseUint(lockID, 10, 32)
	if err != nil {
		return err
	}
	// DBMS_LOCK numeric IDs occupy 0..1073741823; hashing only risks contention.
	o.lockID = int64(numericID & 0x3fffffff)
	return o.ensureVersionTable()
}

// Open opens a go-ora URL, filtering migrate-specific x-* parameters.
func (*Oracle) Open(raw string) (database.Driver, error) {
	dsn, config, err := parseURL(raw)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("oracle", dsn)
	if err != nil {
		return nil, err
	}
	d, err := WithInstance(db, config)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return d, nil
}

// Close releases the migration lock and closes the session and owned pool.
func (o *Oracle) Close() error {
	var unlockErr error
	if o.isLocked.Load() {
		unlockErr = o.Unlock()
	}
	connErr := o.conn.Close()
	if errors.Is(connErr, sql.ErrConnDone) {
		connErr = nil
	}
	return errors.Join(unlockErr, connErr, o.db.Close())
}

// Lock acquires a session lock that survives Oracle's implicit DDL commits.
func (o *Oracle) Lock() error {
	return database.CasRestoreOnErr(&o.isLocked, false, true, database.ErrLocked, func() error {
		code := -1
		query := `BEGIN :1 := DBMS_LOCK.REQUEST(id => :2, lockmode => 6, timeout => 0, release_on_commit => FALSE); END;`
		if err := o.exec(query, sql.Out{Dest: &code}, o.lockID); err != nil {
			return err
		}
		switch code {
		case 0:
			return nil
		case 1, 4:
			return database.ErrLocked
		default:
			return fmt.Errorf("oracle: DBMS_LOCK.REQUEST returned %d", code)
		}
	})
}

// Unlock releases the session lock; a failed release can be retried.
func (o *Oracle) Unlock() error {
	return database.CasRestoreOnErr(&o.isLocked, true, false, database.ErrNotLocked, func() error {
		code := -1
		query := `BEGIN :1 := DBMS_LOCK.RELEASE(:2); END;`
		if err := o.exec(query, sql.Out{Dest: &code}, o.lockID); err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("oracle: DBMS_LOCK.RELEASE returned %d", code)
		}
		return nil
	})
}

// Run frames Oracle SQL/PLSQL statements and executes them without rewriting.
func (o *Oracle) Run(migration io.Reader) error {
	script, err := io.ReadAll(migration)
	if err != nil {
		return err
	}
	statements, err := splitStatements(string(script))
	if err != nil {
		return err
	}
	for _, statement := range statements {
		if err := o.exec(statement); err != nil {
			return err
		}
	}
	return nil
}

// SetVersion atomically persists version and dirty state, including dirty -1.
func (o *Oracle) SetVersion(version int, dirty bool) (err error) {
	if version < database.NilVersion {
		return fmt.Errorf("oracle: version must be at least %d", database.NilVersion)
	}
	ctx, cancel := o.operationContext()
	defer cancel()
	tx, err := o.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
		}
	}()
	query := `DELETE FROM ` + o.versionTable()
	if _, err := tx.ExecContext(ctx, query); err != nil {
		return queryError(query, err)
	}
	if version >= 0 || dirty {
		dirtyValue := 0
		if dirty {
			dirtyValue = 1
		}
		query = `INSERT INTO ` + o.versionTable() + ` (version, dirty) VALUES (:1, :2)`
		if _, err := tx.ExecContext(ctx, query, version, dirtyValue); err != nil {
			return queryError(query, err)
		}
	}
	return tx.Commit()
}

// Version returns NilVersion for an empty version table.
func (o *Oracle) Version() (int, bool, error) {
	ctx, cancel := o.operationContext()
	defer cancel()
	var version, dirty int
	query := `SELECT version, dirty FROM ` + o.versionTable() + ` WHERE ROWNUM = 1`
	err := o.conn.QueryRowContext(ctx, query).Scan(&version, &dirty)
	if errors.Is(err, sql.ErrNoRows) {
		return database.NilVersion, false, nil
	}
	if err != nil {
		return 0, false, queryError(query, err)
	}
	return version, dirty != 0, nil
}

func (o *Oracle) ensureVersionTable() (err error) {
	if err := o.Lock(); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, o.Unlock()) }()
	ctx, cancel := o.operationContext()
	defer cancel()
	query := `SELECT COUNT(*) FROM ALL_TABLES WHERE OWNER = :1 AND TABLE_NAME = :2`
	var count int
	if err := o.conn.QueryRowContext(ctx, query, o.schema, o.config.MigrationsTable).Scan(&count); err != nil {
		return queryError(query, err)
	}
	if count > 0 {
		return nil
	}
	return o.exec(`CREATE TABLE ` + o.versionTable() + ` (version NUMBER(19) NOT NULL PRIMARY KEY, dirty NUMBER(1) NOT NULL CHECK (dirty IN (0, 1)))`)
}

func (o *Oracle) versionTable() string {
	return quoteIdentifier(o.schema) + "." + quoteIdentifier(o.config.MigrationsTable)
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func (o *Oracle) operationContext() (context.Context, context.CancelFunc) {
	if o.config.StatementTimeout > 0 {
		return context.WithTimeout(context.Background(), o.config.StatementTimeout)
	}
	return context.WithCancel(context.Background())
}

func (o *Oracle) exec(query string, args ...any) error {
	ctx, cancel := o.operationContext()
	defer cancel()
	if _, err := o.conn.ExecContext(ctx, query, args...); err != nil {
		return queryError(query, err)
	}
	return nil
}

func queryError(query string, err error) error {
	return &database.Error{OrigErr: err, Query: []byte(query)}
}
