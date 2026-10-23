package acceptance

import (
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	opq "gitcode.com/opengauss/openGauss-connector-go-pq"
	gauss "github.com/HuaweiCloudDeveloper/gaussdb-go"
	"github.com/HuaweiCloudDeveloper/gaussdb-go/stdlib"
	ob "github.com/cocosip/go-oceanbase"
	dmDriver "github.com/cocosip/migrate-plus/database/dm"
	gaussDriver "github.com/cocosip/migrate-plus/database/gaussdb"
	obDriver "github.com/cocosip/migrate-plus/database/oceanbase"
	openDriver "github.com/cocosip/migrate-plus/database/opengauss"
	oraDriver "github.com/cocosip/migrate-plus/database/oracle"
	"github.com/golang-migrate/migrate/v4/database"
)

const (
	driverOracle    = "oracle"
	driverDM        = "dm"
	driverGaussDB   = "gaussdb"
	driverOpenGauss = "opengauss"
	driverOceanBase = "oceanbase"
	modeMySQL       = "mysql"
	modePG          = "pg"
	modeA           = "a"
	modeNative      = "native"
	nativeTrue      = "true"
	nativeFalse     = "false"
)

type connection struct {
	db             *sql.DB
	conn           *sql.Conn
	info           serverInfo
	target         Target
	dsn            string
	server, tenant string
}

type columnInfo struct {
	Type                     string
	Nullable                 bool
	Length, Precision, Scale int64
	Default                  string
}

func connect(ctx context.Context, t Target, dsn string) (*connection, error) {
	if !safeName.MatchString(t.Schema) || forbiddenSchema(t.Schema) {
		return nil, fmt.Errorf("require an explicit non-system disposable schema")
	}
	timeout := t.statementTimeout()
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	db, err := nativePool(t, dsn, timeout)
	if err != nil {
		return nil, err
	}
	c := &connection{db: db, target: t, dsn: dsn}
	c.conn, err = db.Conn(ctx)
	if err == nil {
		c.info, c.server, c.tenant, err = c.identity(ctx)
	}
	if err == nil {
		var objects []object
		objects, err = c.inventory(ctx)
		if err == nil && len(objects) != 0 {
			err = fmt.Errorf("disposable schema is not empty (%d root objects)", len(objects))
		}
	}
	if err != nil {
		return nil, errors.Join(err, c.close())
	}
	return c, nil
}

func nativePool(t Target, dsn string, timeout time.Duration) (*sql.DB, error) {
	raw, err := nativeDSN(t, dsn, timeout)
	if err != nil {
		return nil, err
	}
	var db *sql.DB
	switch t.Driver {
	case driverOracle, driverDM:
		db, err = sql.Open(t.Driver, raw)
	case driverGaussDB:
		var cfg *gauss.ConnConfig
		cfg, err = gauss.ParseConfig(raw)
		if err == nil {
			cfg.ConnectTimeout = timeout
			db = stdlib.OpenDB(*cfg)
		}
	case driverOpenGauss:
		var cfg *opq.Config
		cfg, err = opq.ParseConfig(raw)
		if err == nil {
			cfg.ConnectTimeout = timeout
			var connector *opq.Connector
			connector, err = opq.NewConnectorConfig(cfg)
			if err == nil {
				db = sql.OpenDB(connector)
			}
		}
	case driverOceanBase:
		var cfg ob.Config
		cfg, err = ob.ParseDSN(raw)
		if err == nil {
			if cfg.CreateDatabaseIfMissing || len(cfg.Session.InitSQL) != 0 || cfg.Routing.Role == ob.RoleReadOnly {
				return nil, fmt.Errorf("native connection must use an existing writable target without initialization SQL")
			}
			cfg.AllowMultiStatements = true
			cfg.ConnectTimeout = timeout
			cfg.ReadTimeout = timeout
			var connector sqldriver.Connector
			connector, err = ob.NewConnector(cfg)
			if err == nil {
				db = sql.OpenDB(connector)
			}
		}
	default:
		return nil, fmt.Errorf("unsupported native driver")
	}
	if err != nil {
		return nil, fmt.Errorf("invalid native connection configuration")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

// DM credentials are raw bytes in its DSN grammar, including percent and question marks.
func dsnParts(raw string) (string, string) {
	if i := strings.LastIndexByte(raw, '?'); i >= 0 && strings.Contains(raw[:i], "@") {
		return raw[:i], raw[i+1:]
	}
	return raw, ""
}

func nativeDSN(t Target, raw string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		return "", fmt.Errorf("native timeout must be positive")
	}
	if !strings.HasPrefix(raw, t.Driver+"://") {
		return "", fmt.Errorf("DSN must use the selected driver scheme")
	}
	if t.Driver == driverDM {
		return nativeDM(raw, timeout)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Fragment != "" || u.Host == "" {
		return "", fmt.Errorf("invalid native URL")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", fmt.Errorf("invalid native query")
	}
	seen := map[string]bool{}
	for key, values := range q {
		lower := strings.ToLower(key)
		if len(values) != 1 || seen[lower] || strings.HasPrefix(lower, "x-") {
			return "", fmt.Errorf("duplicate or migration-specific native option")
		}
		seen[lower] = true
		if ((lower == "dbaprivilege" || lower == "dba privilege") && values[0] != "") || lower == "initsql" || lower == "sessioninitsql" {
			return "", fmt.Errorf("privileged or executable initialization option forbidden")
		}
	}
	seconds := strconv.FormatInt(max(1, int64(timeout/time.Second)), 10)
	switch t.Driver {
	case driverOracle:
		for key := range q {
			switch strings.ToLower(key) {
			case "timeout", "read timeout", "connect timeout", "connection timeout", "socket timeout":
				q.Del(key)
			}
		}
		q.Set("CONNECTION TIMEOUT", seconds)
		q.Set("SOCKET TIMEOUT", seconds)
	case driverGaussDB, driverOpenGauss:
		q.Set("connect_timeout", seconds)
	case driverOceanBase:
		cfg, parseErr := ob.ParseDSN(raw)
		if parseErr != nil || cfg.CreateDatabaseIfMissing || len(cfg.Session.InitSQL) != 0 || cfg.Routing.Role == ob.RoleReadOnly {
			return "", fmt.Errorf("require existing writable OceanBase target without initialization SQL")
		}
		if q.Has("allowMultiStatements") && q.Get("allowMultiStatements") != nativeTrue {
			return "", fmt.Errorf("migrations require allowMultiStatements=true")
		}
		q.Set("allowMultiStatements", nativeTrue)
		q.Set("connectTimeout", timeout.String())
		q.Set("readTimeout", timeout.String())
	default:
		return "", fmt.Errorf("unsupported native driver")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func nativeDM(raw string, timeout time.Duration) (string, error) {
	base, query := dsnParts(raw)
	at := strings.LastIndexByte(base, '@')
	if at < 5 || !strings.Contains(base[5:at], ":") || at == len(base)-1 {
		return "", fmt.Errorf("invalid native DM DSN")
	}
	host, path, _ := strings.Cut(base[at+1:], "/")
	if host == "" || strings.ContainsAny(host, "?&\r\n") || strings.ContainsAny(path, "/?&\r\n") {
		return "", fmt.Errorf("invalid native DM host or schema path")
	}
	required := map[string]string{"autocommit": nativeTrue, "rwseparate": "0", "doswitch": "0", "driverreconnect": nativeFalse, "maxrows": "0", "enrscache": nativeFalse, "escapeprocess": nativeFalse}
	seen := map[string]bool{}
	var options []string
	for _, item := range strings.Split(query, "&") {
		if item == "" {
			continue
		}
		key, value, ok := strings.Cut(item, "=")
		lower := strings.ToLower(key)
		if !ok || key == "" || strings.ContainsAny(key, "%?\r\n") || seen[lower] || strings.HasPrefix(lower, "x-") {
			return "", fmt.Errorf("malformed, duplicate or migration-specific DM option")
		}
		seen[lower] = true
		switch lower {
		case "user", "username", "password", "pwd":
			return "", fmt.Errorf("native DM credentials must appear only in userinfo")
		}
		if want, guarded := required[lower]; guarded {
			matches := value == want
			if want == nativeTrue || want == nativeFalse {
				b, e := strconv.ParseBool(value)
				matches = e == nil && b == (want == nativeTrue)
			}
			if !matches {
				return "", fmt.Errorf("unsafe native DM session option")
			}
			continue
		}
		if lower != "sockettimeout" && lower != "connecttimeout" {
			options = append(options, item)
		}
	}
	options = append(options, "autoCommit=true", "rwSeparate=0", "doSwitch=0", "driverReconnect=false", "maxRows=0", "enRsCache=false", "escapeProcess=false")
	seconds := strconv.FormatInt(max(1, int64(timeout/time.Second)), 10)
	options = append(options, "socketTimeout="+seconds, "connectTimeout="+seconds)
	return base + "?" + strings.Join(options, "&"), nil
}

func (c *connection) migrationURL(table string, timeout time.Duration) (string, error) {
	if !safeName.MatchString(table) || timeout < time.Millisecond {
		return "", fmt.Errorf("invalid migration table or timeout")
	}
	raw, err := nativeDSN(c.target, c.dsn, timeout)
	if err != nil {
		return "", err
	}
	options := "x-migrations-table=" + table + "&x-statement-timeout=" + strconv.FormatInt(timeout.Milliseconds(), 10)
	if c.target.Driver == driverDM {
		return raw + "&" + options, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid migration URL")
	}
	q := u.Query()
	q.Set("x-migrations-table", table)
	q.Set("x-statement-timeout", strconv.FormatInt(timeout.Milliseconds(), 10))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (c *connection) driver(table string, timeout time.Duration) (database.Driver, error) {
	if !safeName.MatchString(table) || timeout <= 0 {
		return nil, fmt.Errorf("invalid migration table or timeout")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := c.checkIdentity(ctx); err != nil {
		return nil, err
	}
	db, err := nativePool(c.target, c.dsn, timeout)
	if err != nil {
		return nil, err
	}
	probe := &connection{db: db, target: c.target, dsn: c.dsn, info: c.info, server: c.server, tenant: c.tenant}
	probe.conn, err = db.Conn(ctx)
	if err == nil {
		err = probe.checkIdentity(ctx)
	}
	if err != nil {
		return nil, errors.Join(err, probe.close())
	}
	if err = probe.conn.Close(); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	var driver database.Driver
	switch c.target.Driver {
	case driverOracle:
		driver, err = oraDriver.WithInstance(db, &oraDriver.Config{MigrationsTable: table, StatementTimeout: timeout})
	case driverDM:
		driver, err = dmDriver.WithInstance(db, &dmDriver.Config{MigrationsTable: table, StatementTimeout: timeout})
	case driverGaussDB:
		driver, err = gaussDriver.WithInstance(db, &gaussDriver.Config{MigrationsTable: table, StatementTimeout: timeout})
	case driverOpenGauss:
		driver, err = openDriver.WithInstance(db, &openDriver.Config{MigrationsTable: table, StatementTimeout: timeout})
	case driverOceanBase:
		driver, err = obDriver.WithInstance(db, &obDriver.Config{MigrationsTable: table, StatementTimeout: timeout})
	}
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return driver, nil
}

func (c *connection) qualified(name string) string {
	quote := "\""
	if c.target.Driver == driverOceanBase && c.target.Mode == modeMySQL {
		quote = "`"
	}
	escape := func(s string) string { return quote + strings.ReplaceAll(s, quote, quote+quote) + quote }
	if c.target.Driver == driverOracle || c.target.Driver == driverDM || (c.target.Driver == driverOceanBase && c.target.Mode == driverOracle) {
		name = strings.ToUpper(name)
	}
	return escape(c.info.Schema) + "." + escape(name)
}

func (c *connection) close() error {
	var connErr, dbErr error
	if c.conn != nil {
		connErr = c.conn.Close()
		if errors.Is(connErr, sql.ErrConnDone) {
			connErr = nil
		}
	}
	if c.db != nil {
		dbErr = c.db.Close()
	}
	return errors.Join(connErr, dbErr)
}

func (c *connection) checkIdentity(ctx context.Context) error {
	info, server, tenant, err := c.identity(ctx)
	if err != nil {
		return err
	}
	if info != c.info || server != c.server || tenant != c.tenant {
		return fmt.Errorf("native session identity changed")
	}
	return nil
}

func (c *connection) peerCompatible(other *connection) error {
	if other == nil || c.target.Driver != other.target.Driver || c.target.Mode != other.target.Mode || c.info.Schema == other.info.Schema || c.server == "" || c.server != other.server || c.info.Database == "" || c.info.Database != other.info.Database || c.tenant != other.tenant {
		return fmt.Errorf("paired schemas require the same verified server, database and tenant")
	}
	return nil
}

func (c *connection) validateIdentity(info serverInfo, admin bool) error {
	if info.Product != c.target.Driver || info.Version == "" || info.Mode != c.target.Mode || info.Schema != c.target.Schema || info.Database == "" || info.User == "" || forbiddenSchema(info.Schema) || forbiddenSchema(strings.Split(info.User, "@")[0]) || admin {
		return fmt.Errorf("native product, mode, disposable schema or non-administrator identity mismatch")
	}
	switch strings.ToUpper(strings.Split(info.User, "@")[0]) {
	case "ROOT", "POSTGRES", "GAUSSDB", "DBA":
		return fmt.Errorf("administrator connection forbidden")
	}
	return nil
}

func sortObjects(objects []object) {
	sort.Slice(objects, func(i, j int) bool {
		if objects[i].Name == objects[j].Name {
			return objects[i].Kind < objects[j].Kind
		}
		return objects[i].Name < objects[j].Name
	})
}
