package acceptance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	ob "github.com/cocosip/go-oceanbase"
)

func (c *connection) identity(ctx context.Context) (serverInfo, string, string, error) {
	info := serverInfo{Product: c.target.Driver}
	var server, tenant string
	var admin bool
	switch c.target.Driver {
	case driverOracle:
		var banners []string
		err := c.strings(ctx, `SELECT BANNER FROM V$VERSION`, func(s string) { banners = append(banners, s) })
		if err != nil {
			return info, "", "", err
		}
		info.Version = strings.Join(banners, "; ")
		if !strings.Contains(strings.ToLower(info.Version), "oracle database") || strings.Contains(strings.ToLower(info.Version), driverOceanBase) {
			return info, "", "", fmt.Errorf("server does not identify as Oracle Database")
		}
		info.Mode = driverOracle
		var count int
		err = c.conn.QueryRowContext(ctx, `SELECT SYS_CONTEXT('USERENV','CURRENT_SCHEMA'), SYS_CONTEXT('USERENV','SESSION_USER'), SYS_CONTEXT('USERENV','DB_UNIQUE_NAME'), SYS_CONTEXT('USERENV','CON_NAME'), SYS_CONTEXT('USERENV','SERVER_HOST'), (SELECT COUNT(*) FROM SESSION_ROLES WHERE ROLE IN ('DBA','SYSDBA')) FROM dual`).Scan(&info.Schema, &info.User, &info.Database, &tenant, &server, &count)
		if err != nil {
			return info, "", "", err
		}
		admin = count > 0
	case driverDM:
		var banners []string
		if err := c.strings(ctx, `SELECT BANNER FROM V$VERSION`, func(s string) { banners = append(banners, s) }); err != nil {
			return info, "", "", err
		}
		info.Version = strings.Join(banners, "; ")
		lower := strings.ToLower(info.Version)
		if !strings.Contains(lower, "dm8") && !strings.Contains(lower, "dm database server") {
			return info, "", "", fmt.Errorf("server does not identify as DM8")
		}
		var mode, count int
		err := c.conn.QueryRowContext(ctx, `SELECT SF_GET_SCHEMA_NAME_BY_ID(CURRENT_SCHID()), USER, SF_GET_PARA_VALUE(2,'COMPATIBLE_MODE'), (SELECT NAME FROM V$DATABASE), (SELECT COUNT(*) FROM SESSION_ROLES WHERE ROLE IN ('DBA','SYSDBA')) FROM DUAL`).Scan(&info.Schema, &info.User, &mode, &info.Database, &count)
		if err != nil {
			return info, "", "", err
		}
		admin = count > 0
		info.Mode = map[int]string{0: modeNative, 2: driverOracle, 4: modeMySQL, 7: modePG}[mode]
		if err := c.conn.QueryRowContext(ctx, `SELECT NAME || ':' || HOST_NAME FROM V$INSTANCE`).Scan(&server); err != nil {
			return info, "", "", err
		}
		server = c.endpoint() + "/" + server
	case driverGaussDB, driverOpenGauss:
		var product string
		err := c.conn.QueryRowContext(ctx, `SELECT pg_catalog.version(), pg_catalog.current_database(), pg_catalog.current_schema(), CURRENT_USER, pg_catalog.current_setting('sql_compatibility'), COALESCE(pg_catalog.inet_server_addr()::text,'local') || ':' || pg_catalog.inet_server_port()::text, (SELECT rolsuper FROM pg_catalog.pg_roles WHERE rolname = CURRENT_USER)`).Scan(&product, &info.Database, &info.Schema, &info.User, &info.Mode, &server, &admin)
		if err != nil {
			return info, "", "", err
		}
		info.Version = product
		info.Mode = strings.ToLower(info.Mode)
		lower := strings.ToLower(product)
		if c.target.Driver == driverGaussDB && !strings.Contains(lower, driverGaussDB) || c.target.Driver == driverOpenGauss && (!strings.Contains(lower, driverOpenGauss) || strings.Contains(lower, driverGaussDB)) {
			return info, "", "", fmt.Errorf("native Gauss product mismatch")
		}
	case driverOceanBase:
		var err error
		info, server, tenant, admin, err = c.oceanbaseIdentity(ctx)
		if err != nil {
			return info, "", "", err
		}
	default:
		return info, "", "", fmt.Errorf("unsupported identity adapter")
	}
	if err := c.validateIdentity(info, admin); err != nil {
		return info, "", "", err
	}
	if server == "" {
		return info, "", "", fmt.Errorf("server identity is unavailable")
	}
	return info, server, tenant, nil
}

func (c *connection) endpoint() string {
	base, _ := dsnParts(c.dsn)
	at := strings.LastIndexByte(base, '@')
	if at < 0 {
		return ""
	}
	host, _, _ := strings.Cut(base[at+1:], "/")
	return strings.ToLower(host)
}

func (c *connection) strings(ctx context.Context, query string, visit func(string)) (err error) {
	rows, err := c.conn.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return err
		}
		visit(value)
	}
	return rows.Err()
}

func (c *connection) inventory(ctx context.Context) (_ []object, err error) {
	var query string
	var args []any
	switch c.target.Driver {
	case driverOracle:
		query = `SELECT object_name, object_type FROM ALL_OBJECTS WHERE owner = :1 AND subobject_name IS NULL AND generated = 'N' AND secondary = 'N'
AND object_type NOT IN ('INDEX','LOB','PACKAGE BODY','TYPE BODY')
AND object_name NOT IN (SELECT table_name FROM ALL_NESTED_TABLES WHERE owner = :2)`
		args = []any{c.info.Schema, c.info.Schema}
	case driverDM:
		query = `SELECT o.NAME, o.SUBTYPE$ FROM SYS.SYSOBJECTS o WHERE o.SCHID = CURRENT_SCHID() AND o.TYPE$ = 'SCHOBJ'
AND (o.PID = -1 OR o.SUBTYPE$ IN ('VIEW','SEQ','SYNOM','PKG','PROC','TYPE','CLASS') OR (o.SUBTYPE$ = 'TRIG' AND BITAND(o.INFO1,2) = 0))`
	case driverGaussDB, driverOpenGauss:
		query = `SELECT c.relname, CASE c.relkind WHEN 'r' THEN 'TABLE' WHEN 'p' THEN 'TABLE' WHEN 'v' THEN 'VIEW' WHEN 'm' THEN 'MATERIALIZED VIEW' WHEN 'S' THEN 'SEQUENCE' WHEN 'L' THEN 'SEQUENCE' ELSE 'UNSUPPORTED' END
FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relkind NOT IN ('i','I','t','c')
AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d WHERE d.classid = 'pg_catalog.pg_class'::regclass AND d.objid = c.oid AND d.refclassid = 'pg_catalog.pg_class'::regclass AND d.deptype IN ('i','a'))
UNION ALL SELECT p.proname, CASE p.prokind WHEN 'f' THEN 'FUNCTION' WHEN 'p' THEN 'PROCEDURE' ELSE 'UNSUPPORTED' END FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace WHERE n.nspname=$1
UNION ALL SELECT t.typname, CASE WHEN t.typtype='d' THEN 'DOMAIN' ELSE 'TYPE' END FROM pg_catalog.pg_type t JOIN pg_catalog.pg_namespace n ON n.oid=t.typnamespace LEFT JOIN pg_catalog.pg_class c ON c.oid=t.typrelid WHERE n.nspname=$1 AND (t.typrelid=0 OR c.relkind='c') AND NOT (t.typelem<>0 AND t.typlen=-1)
UNION ALL SELECT pg_catalog.pg_describe_object(d.classid,d.objid,0), 'UNSUPPORTED' FROM pg_catalog.pg_depend d JOIN pg_catalog.pg_namespace n ON n.oid=d.refobjid WHERE n.nspname=$1 AND d.refclassid='pg_catalog.pg_namespace'::regclass AND d.deptype='n' AND d.classid NOT IN ('pg_catalog.pg_class'::regclass,'pg_catalog.pg_proc'::regclass,'pg_catalog.pg_type'::regclass)`
		args = []any{c.info.Schema}
	case driverOceanBase:
		if c.target.Mode == driverOracle {
			query = `SELECT object_name, object_type FROM ALL_OBJECTS WHERE owner = :1 AND subobject_name IS NULL AND object_type NOT IN ('INDEX','LOB','PACKAGE BODY','TYPE BODY')`
			args = []any{c.info.Schema}
		} else {
			query = `SELECT table_name, CASE table_type WHEN 'BASE TABLE' THEN 'TABLE' WHEN 'VIEW' THEN 'VIEW' ELSE table_type END FROM information_schema.tables WHERE table_schema = ?
UNION ALL SELECT routine_name,routine_type FROM information_schema.routines WHERE routine_schema = ?
UNION ALL SELECT trigger_name,'TRIGGER' FROM information_schema.triggers WHERE trigger_schema = ?
UNION ALL SELECT event_name,'EVENT' FROM information_schema.events WHERE event_schema = ?`
			args = []any{c.info.Schema, c.info.Schema, c.info.Schema, c.info.Schema}
		}
	default:
		return nil, fmt.Errorf("unsupported inventory adapter")
	}
	rows, err := c.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var objects []object
	for rows.Next() {
		var o object
		if err := rows.Scan(&o.Name, &o.Kind); err != nil {
			return nil, err
		}
		if o.Name == "" || o.Kind == "" {
			return nil, fmt.Errorf("invalid root catalog object")
		}
		objects = append(objects, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortObjects(objects)
	return objects, nil
}

func (c *connection) column(ctx context.Context, table, column string) (columnInfo, error) {
	var query string
	var args []any
	switch c.target.Driver {
	case driverOracle, driverDM:
		query = oracleColumnSQL
		if c.target.Driver == driverDM {
			query = strings.NewReplacer(":1", "?", ":2", "?", ":3", "?").Replace(query)
		}
		args = []any{c.info.Schema, strings.ToUpper(table), strings.ToUpper(column)}
	case driverOceanBase:
		if c.target.Mode == driverOracle {
			query = oracleColumnSQL
			args = []any{c.info.Schema, strings.ToUpper(table), strings.ToUpper(column)}
		} else {
			query = `SELECT DATA_TYPE, IS_NULLABLE, CHARACTER_MAXIMUM_LENGTH, NUMERIC_PRECISION, NUMERIC_SCALE, COLUMN_DEFAULT FROM information_schema.columns WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = ?`
			args = []any{c.info.Schema, table, column}
		}
	case driverGaussDB, driverOpenGauss:
		query = `SELECT COALESCE(domain_name,CASE WHEN data_type='USER-DEFINED' THEN udt_name ELSE data_type END),is_nullable,character_maximum_length,numeric_precision,numeric_scale,column_default FROM information_schema.columns WHERE table_schema=$1 AND table_name=$2 AND column_name=$3`
		args = []any{c.info.Schema, table, column}
	default:
		return columnInfo{}, fmt.Errorf("unsupported column adapter")
	}
	var info columnInfo
	var nullable string
	var length, precision, scale sql.NullInt64
	var defaultValue sql.NullString
	err := c.conn.QueryRowContext(ctx, query, args...).Scan(&info.Type, &nullable, &length, &precision, &scale, &defaultValue)
	info.Nullable = nullable == "Y" || nullable == "YES"
	info.Length = length.Int64
	info.Precision = precision.Int64
	info.Scale = scale.Int64
	info.Default = defaultValue.String
	return info, err
}

const oracleColumnSQL = `SELECT DATA_TYPE, NULLABLE, CASE WHEN DATA_TYPE IN ('CHAR','VARCHAR','VARCHAR2','NCHAR','NVARCHAR','NVARCHAR2') THEN CHAR_LENGTH ELSE DATA_LENGTH END, DATA_PRECISION, DATA_SCALE, DATA_DEFAULT FROM ALL_TAB_COLUMNS WHERE OWNER = :1 AND TABLE_NAME = :2 AND COLUMN_NAME = :3`

func (c *connection) oceanbaseIdentity(ctx context.Context) (serverInfo, string, string, bool, error) {
	info := serverInfo{Product: c.target.Driver}
	var server, tenant string
	var admin bool
	rows, err := ob.Metadata(ctx, c.conn, ob.MetadataFilter{Kind: ob.MetadataServerInfo})
	if err != nil {
		return info, "", "", false, err
	}
	var parameters, types int
	if !rows.Next() {
		return info, "", "", false, errors.Join(sql.ErrNoRows, rows.Err(), rows.Close())
	}
	err = rows.Scan(&info.Version, &info.Mode, &parameters, &types)
	if rows.Next() {
		err = errors.Join(err, fmt.Errorf("ambiguous OceanBase server identity"))
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return info, "", "", false, err
	}
	info.Mode = strings.ToLower(info.Mode)
	var tenantName string
	query := `SELECT DATABASE(), CURRENT_USER()`
	tenantQuery := `SELECT TENANT_ID, TENANT_NAME FROM oceanbase.DBA_OB_TENANTS`
	if info.Mode == driverOracle {
		query = `SELECT SYS_CONTEXT('USERENV','CURRENT_SCHEMA'), SYS_CONTEXT('USERENV','SESSION_USER') FROM dual`
		tenantQuery = `SELECT TENANT_ID, TENANT_NAME FROM DBA_OB_TENANTS`
	}
	if err := c.conn.QueryRowContext(ctx, query).Scan(&info.Schema, &info.User); err != nil {
		return info, "", "", false, err
	}
	rows, err = c.conn.QueryContext(ctx, tenantQuery)
	if err != nil {
		return info, "", "", false, err
	}
	if !rows.Next() {
		return info, "", "", false, errors.Join(sql.ErrNoRows, rows.Err(), rows.Close())
	}
	err = rows.Scan(&tenant, &tenantName)
	if rows.Next() {
		err = errors.Join(err, fmt.Errorf("tenant identity is ambiguous; system tenants are forbidden"))
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return info, "", "", false, err
	}
	if tenant == "" || tenant == "1" || strings.EqualFold(tenantName, "sys") {
		return info, "", "", false, fmt.Errorf("system or unknown OceanBase tenant forbidden")
	}
	// OceanBase MySQL database is the schema; tenant is the cross-schema database boundary.
	info.Database = tenant
	clusterQuery := `SELECT DISTINCT VALUE FROM oceanbase.GV$OB_PARAMETERS WHERE NAME = 'cluster_id'`
	if info.Mode == driverOracle {
		clusterQuery = `SELECT DISTINCT VALUE FROM GV$OB_PARAMETERS WHERE NAME = 'cluster_id'`
	}
	clusterID, err := c.singleIdentityValue(ctx, clusterQuery)
	if err != nil {
		return info, "", "", false, err
	}
	server = c.endpoint() + "/" + clusterID
	if info.Mode == driverOracle {
		var count int
		if err := c.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM SESSION_ROLES WHERE ROLE IN ('DBA','SYSDBA')`).Scan(&count); err != nil {
			return info, "", "", false, err
		}
		admin = count > 0
	} else {
		if err := c.strings(ctx, `SHOW GRANTS FOR CURRENT_USER`, func(grant string) { admin = admin || globalAdminGrant(grant) }); err != nil {
			return info, "", "", false, err
		}
	}
	return info, server, tenant, admin, nil
}

func globalAdminGrant(grant string) bool {
	privileges, scope, ok := strings.Cut(strings.ToUpper(grant), " ON ")
	if !ok || !strings.HasPrefix(scope, "*.* ") {
		return false
	}
	for _, privilege := range []string{"ALL PRIVILEGES", "SUPER", "CREATE USER", "GRANT OPTION", "ALTER SYSTEM"} {
		if strings.Contains(privileges, privilege) || strings.Contains(scope, "WITH GRANT OPTION") {
			return true
		}
	}
	return false
}

func (c *connection) singleIdentityValue(ctx context.Context, query string) (string, error) {
	var values []string
	if err := c.strings(ctx, query, func(value string) { values = append(values, value) }); err != nil {
		return "", err
	}
	if len(values) != 1 || values[0] == "" {
		return "", fmt.Errorf("server identity is missing or ambiguous")
	}
	return values[0], nil
}
