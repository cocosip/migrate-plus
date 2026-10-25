package opengauss

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	opq "gitcode.com/opengauss/openGauss-connector-go-pq"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
)

func init() { database.Register("opengauss", &OpenGauss{}) }

const rollbackSQL = "ROLLBACK"

const transactionProbeSQL = "SAVEPOINT migrate_plus_transaction_probe"

const scriptSettingsSQL = `SELECT pg_catalog.current_setting('sql_compatibility'), pg_catalog.current_setting('standard_conforming_strings'), pg_catalog.current_setting('client_encoding')`

// OpenGauss reserves one session for migration SQL, state and locking.
type OpenGauss struct {
	db           *sql.DB
	conn         *sql.Conn
	config       Config
	caps         capabilities
	databaseName string
	schema       string
	schemaID     int64
	lockID       int64
	isLocked     atomic.Bool
	broken       atomic.Bool
	closed       atomic.Bool
	closeOnce    sync.Once
	closeErr     error
	lockMu       sync.Mutex
	lockCtx      context.Context
	stopLock     context.CancelFunc
}

var _ database.Driver = (*OpenGauss)(nil)

// WithInstance takes ownership of the provided pool on success. Close closes the session and
// pool; construction failure leaves the pool open.
func WithInstance(instance *sql.DB, config *Config) (_ database.Driver, err error) {
	if instance == nil {
		return nil, fmt.Errorf("opengauss: nil database instance")
	}
	normalized, err := prepareConfig(config)
	if err != nil {
		return nil, err
	}
	g := &OpenGauss{db: instance, config: normalized}
	ctx, cancel := g.operationContext()
	defer cancel()
	g.conn, err = instance.Conn(ctx)
	if err != nil {
		return nil, err
	}
	g.lockCtx, g.stopLock = context.WithCancel(context.Background())
	defer func() {
		if err != nil {
			g.discard()
		}
	}()
	if err := g.initialize(); err != nil {
		return nil, err
	}
	return g, nil
}

func (g *OpenGauss) initialize() error {
	var product, mode, standard, readOnly, encoding string
	var identifierLimit int
	query := `SELECT pg_catalog.version(), pg_catalog.current_database(), pg_catalog.current_schema(),
pg_catalog.current_setting('sql_compatibility'), pg_catalog.current_setting('max_identifier_length')::integer,
pg_catalog.current_setting('standard_conforming_strings'), pg_catalog.current_setting('transaction_read_only'),
pg_catalog.current_setting('client_encoding')`
	if err := g.query(query, func(rows *sql.Rows) error {
		if !rows.Next() {
			return sql.ErrNoRows
		}
		return rows.Scan(&product, &g.databaseName, &g.schema, &mode, &identifierLimit, &standard, &readOnly, &encoding)
	}); err != nil {
		return err
	}
	lower := strings.ToLower(product)
	if g.databaseName == "" || g.schema == "" {
		return fmt.Errorf("opengauss: cannot identify the connected database and schema")
	}
	if strings.Contains(lower, "gaussdb") {
		return fmt.Errorf("opengauss: connected to a Huawei GaussDB product; use the database/gaussdb driver")
	}
	if !strings.Contains(lower, "opengauss") {
		return fmt.Errorf("opengauss: server does not identify as openGauss; PostgreSQL servers need the upstream postgres driver")
	}
	if strings.HasPrefix(strings.ToLower(g.schema), "pg_") || strings.EqualFold(g.schema, "information_schema") {
		return fmt.Errorf("opengauss: migrations cannot target a system schema")
	}
	caps, err := scriptCapabilities(mode, standard, encoding)
	if err != nil {
		return err
	}
	g.caps = caps
	if readOnly != settingOff {
		return fmt.Errorf("opengauss: require a writable session")
	}
	if identifierLimit <= 0 || len(g.config.MigrationsTable) > identifierLimit {
		return fmt.Errorf("opengauss: migration table exceeds the server identifier limit")
	}
	if err := g.query(`SELECT oid::bigint FROM pg_catalog.pg_namespace WHERE nspname = $1`, func(rows *sql.Rows) error {
		if !rows.Next() {
			return sql.ErrNoRows
		}
		return rows.Scan(&g.schemaID)
	}, g.schema); err != nil {
		return err
	}
	if g.schemaID <= 0 {
		return fmt.Errorf("opengauss: invalid schema identity")
	}
	if err := g.checkTopology(); err != nil {
		return err
	}
	lockID, err := database.GenerateAdvisoryLockId(g.databaseName, g.schema, g.config.MigrationsTable)
	if err != nil {
		return err
	}
	g.lockID, err = strconv.ParseInt(lockID, 10, 64)
	if err != nil {
		return err
	}
	return g.ensureVersionTable()
}

func (g *OpenGauss) checkTopology() error {
	count := 0
	query := `SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'pg_catalog' AND c.relname = 'pgxc_node'`
	if err := g.query(query, func(rows *sql.Rows) error { return scanCount(rows, &count) }); err != nil {
		return err
	}
	if count == 0 {
		return nil
	}
	if err := g.query(`SELECT count(*) FROM pg_catalog.pgxc_node WHERE node_type = 'C'`, func(rows *sql.Rows) error {
		return scanCount(rows, &count)
	}); err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("opengauss: distributed coordinator topology requires verified cluster-wide locking and is unsupported")
	}
	return nil
}

func scanCount(rows *sql.Rows, count *int) error {
	if !rows.Next() {
		return sql.ErrNoRows
	}
	return rows.Scan(count)
}

// Open parses native options, removes migration options and opens the official client.
func (*OpenGauss) Open(raw string) (database.Driver, error) {
	native, config, err := parseURL(raw)
	if err != nil {
		return nil, err
	}
	connector, err := opq.NewConnectorConfig(native)
	if err != nil {
		// Native connector errors can retain query passwords even after userinfo redaction.
		return nil, fmt.Errorf("opengauss: invalid native connection configuration")
	}
	db := sql.OpenDB(connector)
	g, err := WithInstance(db, config)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return g, nil
}

// Close cancels lock acquisition, releases held locks, and closes the owned pool.
func (g *OpenGauss) Close() error {
	g.closeOnce.Do(func() {
		g.stopLock()
		g.lockMu.Lock()
		defer g.lockMu.Unlock()
		if g.isLocked.Load() && !g.broken.Load() {
			g.closeErr = g.unlock()
			if g.closeErr != nil {
				g.discard()
			}
		}
		g.closed.Store(true)
		connErr := g.conn.Close()
		if errors.Is(connErr, sql.ErrConnDone) {
			connErr = nil
		}
		g.closeErr = errors.Join(g.closeErr, connErr, g.db.Close())
	})
	return g.closeErr
}

// Lock retries a native exclusive session lock until acquisition, timeout or Close.
func (g *OpenGauss) Lock() error {
	return g.lock(context.Background())
}

func (g *OpenGauss) lock(parent context.Context) error {
	if !g.lockMu.TryLock() {
		return database.ErrLocked
	}
	defer g.lockMu.Unlock()
	if err := g.usable(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if g.config.StatementTimeout > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, g.config.StatementTimeout)
		defer stop()
	}
	stop := context.AfterFunc(g.lockCtx, cancel)
	defer stop()
	return database.CasRestoreOnErr(&g.isLocked, false, true, database.ErrLocked, func() error {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			var acquired bool
			err := g.queryContext(ctx, `SELECT pg_catalog.pg_try_advisory_lock($1::bigint)`, func(rows *sql.Rows) error {
				if !rows.Next() {
					return sql.ErrNoRows
				}
				return rows.Scan(&acquired)
			}, g.lockID)
			if err != nil || acquired {
				return err
			}
			select {
			case <-ctx.Done():
				return errors.Join(database.ErrLocked, ctx.Err())
			case <-ticker.C:
			}
		}
	})
}

// Unlock preserves held state if the server cannot confirm release.
func (g *OpenGauss) Unlock() error {
	g.lockMu.Lock()
	defer g.lockMu.Unlock()
	return g.unlock()
}

func (g *OpenGauss) unlock() error {
	return database.CasRestoreOnErr(&g.isLocked, true, false, database.ErrNotLocked, func() error {
		var released bool
		err := g.query(`SELECT pg_catalog.pg_advisory_unlock($1::bigint)`, func(rows *sql.Rows) error {
			if !rows.Next() {
				return sql.ErrNoRows
			}
			return rows.Scan(&released)
		}, g.lockID)
		if err != nil {
			return err
		}
		if !released {
			return fmt.Errorf("opengauss: server did not confirm advisory lock release")
		}
		return nil
	})
}

// Run sends one unchanged native SQL file after checking unsupported protocol forms and clears
// any transaction the file left open or failed mid-way.
func (g *OpenGauss) Run(migration io.Reader) error {
	if err := g.usable(); err != nil {
		return err
	}
	script, err := io.ReadAll(migration)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(script)) == "" {
		return nil
	}
	var mode, standard, encoding string
	if err := g.query(scriptSettingsSQL, func(rows *sql.Rows) error {
		if !rows.Next() {
			return sql.ErrNoRows
		}
		return rows.Scan(&mode, &standard, &encoding)
	}); err != nil {
		return err
	}
	g.caps, err = scriptCapabilities(mode, standard, encoding)
	if err != nil {
		return err
	}
	if err := validateScript(string(script), g.caps); err != nil {
		return err
	}
	execErr := g.exec(string(script))
	if g.broken.Load() {
		return execErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	confirmedIdle := false
	if execErr == nil {
		// SAVEPOINT succeeds only inside a transaction; SQLSTATE 25P01 means the session is idle.
		_, probeErr := g.conn.ExecContext(ctx, transactionProbeSQL)
		var native interface{ SQLState() string }
		switch {
		case probeErr == nil:
			execErr = fmt.Errorf("opengauss: migration left an open transaction")
		case errors.As(probeErr, &native) && native.SQLState() == "25P01":
			confirmedIdle = true
		default:
			execErr = g.operationError(ctx, transactionProbeSQL, probeErr)
			if g.broken.Load() {
				return execErr
			}
		}
	}
	// A failed script, an open transaction or an uncertain probe leaves the transaction
	// state unknown; only a confirmed idle session needs no rollback.
	if !confirmedIdle {
		if _, rollbackErr := g.conn.ExecContext(ctx, rollbackSQL); rollbackErr != nil {
			g.discard()
			return errors.Join(execErr, g.operationError(ctx, rollbackSQL, rollbackErr))
		}
	}
	return execErr
}

// SetVersion durably replaces migration state with transactional DELETE/INSERT.
func (g *OpenGauss) SetVersion(version int, dirty bool) (err error) {
	if version < database.NilVersion {
		return fmt.Errorf("opengauss: version must be at least %d", database.NilVersion)
	}
	if err := g.usable(); err != nil {
		return err
	}
	ctx, cancel := g.operationContext()
	defer cancel()
	query := `DELETE FROM ` + g.versionTable()
	defer func() { err = g.operationError(ctx, query, err) }()
	tx, err := g.conn.BeginTx(ctx, nil)
	if err != nil {
		g.discard()
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, rollbackErr)
			g.discard()
		}
	}()
	if _, err := tx.ExecContext(ctx, query); err != nil {
		return err
	}
	if version >= 0 || dirty {
		value := 0
		if dirty {
			value = 1
		}
		query = `INSERT INTO ` + g.versionTable() + ` ("version", "dirty") VALUES ($1, $2)`
		if _, err := tx.ExecContext(ctx, query, version, value); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		g.discard()
		return err
	}
	return nil
}

// Version returns NilVersion for empty state and rejects corrupt or ambiguous rows.
func (g *OpenGauss) Version() (version int, dirty bool, err error) {
	version = database.NilVersion
	if err := g.usable(); err != nil {
		return version, false, err
	}
	err = g.query(`SELECT "version", "dirty" FROM `+g.versionTable(), func(rows *sql.Rows) error {
		if !rows.Next() {
			return nil
		}
		var value int
		if err := rows.Scan(&version, &value); err != nil {
			return err
		}
		if version < database.NilVersion || (value != 0 && value != 1) || rows.Next() {
			return fmt.Errorf("opengauss: corrupt or multiple migration state rows")
		}
		dirty = value == 1
		return nil
	})
	return version, dirty, err
}

func (g *OpenGauss) ensureVersionTable() (err error) {
	lockCtx, cancelLock := context.WithTimeout(context.Background(), migrate.DefaultLockTimeout)
	defer cancelLock()
	if err := g.lock(lockCtx); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, g.Unlock()) }()
	var kind, persistence string
	query := `SELECT c.relkind, c.relpersistence FROM pg_catalog.pg_class c WHERE c.relnamespace = $1 AND c.relname = $2`
	if err := g.query(query, func(rows *sql.Rows) error {
		if rows.Next() {
			return rows.Scan(&kind, &persistence)
		}
		return nil
	}, g.schemaID, g.config.MigrationsTable); err != nil {
		return err
	}
	if kind == "" {
		if err := g.exec(`CREATE TABLE ` + g.versionTable() + ` ("version" BIGINT NOT NULL PRIMARY KEY, "dirty" SMALLINT NOT NULL CHECK ("dirty" IN (0, 1)))`); err != nil {
			return err
		}
	} else if kind != "r" || persistence != "p" {
		return fmt.Errorf("opengauss: migration state must be a permanent ordinary table")
	}
	query = `SELECT a.attname, a.atttypid::bigint, a.attnotnull,
EXISTS (SELECT 1 FROM pg_catalog.pg_constraint k WHERE k.conrelid = c.oid AND k.contype = 'p'
AND pg_catalog.array_length(k.conkey, 1) = 1 AND a.attnum = ANY(k.conkey))
FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
WHERE c.relnamespace = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped`
	if err := g.query(query, validateVersionColumns, g.schemaID, g.config.MigrationsTable); err != nil {
		return err
	}
	_, _, err = g.Version()
	return err
}

func validateVersionColumns(rows *sql.Rows) error {
	seen := make(map[string]bool)
	for rows.Next() {
		var name string
		var oid int64
		var notNull, primary bool
		if err := rows.Scan(&name, &oid, &notNull, &primary); err != nil {
			return err
		}
		if name != "version" && name != "dirty" {
			return fmt.Errorf("opengauss: unsupported additional migration state column %q", name)
		}
		valid := name == "version" && oid == 20 && primary || name == "dirty" && (oid == 20 || oid == 21 || oid == 23)
		if seen[name] || !notNull || !valid {
			return fmt.Errorf("opengauss: state requires a BIGINT primary version and non-null integer dirty column")
		}
		seen[name] = true
	}
	if len(seen) != 2 {
		return fmt.Errorf("opengauss: missing migration state columns")
	}
	return nil
}

func (g *OpenGauss) versionTable() string {
	return quoteQualified(g.schema, g.config.MigrationsTable)
}

func (g *OpenGauss) usable() error {
	if g.broken.Load() || g.closed.Load() {
		return fmt.Errorf("opengauss: migration session is closed; construct a new driver: %w", sql.ErrConnDone)
	}
	return nil
}

func (g *OpenGauss) operationContext() (context.Context, context.CancelFunc) {
	if g.config.StatementTimeout > 0 {
		return context.WithTimeout(context.Background(), g.config.StatementTimeout)
	}
	return context.WithCancel(context.Background())
}

func (g *OpenGauss) discard() {
	if g.broken.CompareAndSwap(false, true) {
		g.stopLock()
		_ = g.conn.Raw(func(any) error { return sqldriver.ErrBadConn })
	}
}

func (g *OpenGauss) operationError(ctx context.Context, query string, err error) error {
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	if err == nil {
		return nil
	}
	var native *opq.Error
	var stater interface{ SQLState() string }
	var network net.Error
	fatal := errors.Is(err, sqldriver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network)
	if errors.As(err, &native) {
		fatal = fatal || strings.HasPrefix(native.SQLState(), "08") || native.Severity == "FATAL" || native.Severity == "PANIC"
	} else if errors.As(err, &stater) {
		// Wire-compatible pools from other clients expose connection failures via SQLState.
		fatal = fatal || strings.HasPrefix(stater.SQLState(), "08")
	}
	if fatal {
		g.discard()
	}
	return &database.Error{OrigErr: err, Err: "opengauss operation failed", Query: []byte(query)}
}

func (g *OpenGauss) exec(query string, args ...any) error {
	if err := g.usable(); err != nil {
		return err
	}
	ctx, cancel := g.operationContext()
	defer cancel()
	_, err := g.conn.ExecContext(ctx, query, args...)
	return g.operationError(ctx, query, err)
}

func (g *OpenGauss) query(query string, scan func(*sql.Rows) error, args ...any) error {
	ctx, cancel := g.operationContext()
	defer cancel()
	return g.queryContext(ctx, query, scan, args...)
}

func (g *OpenGauss) queryContext(ctx context.Context, query string, scan func(*sql.Rows) error, args ...any) (err error) {
	if err := g.usable(); err != nil {
		return err
	}
	defer func() { err = g.operationError(ctx, query, err) }()
	rows, err := g.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	if err := scan(rows); err != nil {
		return err
	}
	return rows.Err()
}
