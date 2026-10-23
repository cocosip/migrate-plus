package acceptance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestNativeDSNRequiresMatchingSchemeAndNoMigrationOptions(t *testing.T) {
	for _, dsn := range []string{"postgres://u:p@host/db", "opengauss://u:p@host/db?x-migrations-table=other", "opengauss://u:p@host/db?X-statement-timeout=1", "opengauss://u:p@host/db?password=one&password=two"} {
		if _, err := nativeDSN(Target{Driver: driverOpenGauss}, dsn, time.Second); err == nil {
			t.Errorf("accepted incompatible or conflicting DSN")
		}
	}
}

func TestNativeOracleRejectsPrivilegedLoginOptions(t *testing.T) {
	for _, key := range []string{"DBA+PRIVILEGE", "DBAPrivilege"} {
		if _, err := nativeDSN(Target{Driver: driverOracle}, "oracle://worker:p@host/service?"+key+"=SYSDBA", time.Second); err == nil {
			t.Fatal("privileged Oracle login option accepted")
		}
	}
}

func TestNativeDMPreservesRawCredentialsAndPinsClientInvariants(t *testing.T) {
	raw := "dm://worker:p%+a?ss@host:5236/WORKER?compatibleMode=2"
	got, err := nativeDSN(Target{Driver: driverDM}, raw, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "dm://worker:p%+a?ss@host:5236/WORKER?") {
		t.Fatal("raw credentials changed")
	}
	for _, option := range []string{"autoCommit=true", "rwSeparate=0", "doSwitch=0", "driverReconnect=false", "maxRows=0", "enRsCache=false", "escapeProcess=false", "socketTimeout=3"} {
		if !strings.Contains(got, option) {
			t.Errorf("missing %s", option)
		}
	}
	for _, option := range []string{"autoCommit=false", "rwSeparate=1", "maxRows=1", "driverReconnect=true", "escapeProcess=true"} {
		if _, err := nativeDSN(Target{Driver: driverDM}, raw+"&"+option, time.Second); err == nil {
			t.Errorf("accepted %s", option)
		}
	}
	c := &connection{target: Target{Driver: driverDM}, dsn: raw}
	url, err := c.migrationURL("mp_history", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(url, "dm://worker:p%+a?ss@host:5236/WORKER?") || !strings.Contains(url, "x-migrations-table=mp_history") {
		t.Fatal("production URL corrupted raw DM DSN")
	}
}

func TestNativeDMRejectsQueryCredentialOverrides(t *testing.T) {
	for _, option := range []string{"password=other", "PASSWORD=other@elsewhere", "user=other", "userName=other", "pwd=other"} {
		if _, err := nativeDSN(Target{Driver: driverDM}, "dm://worker:original@host:5236/WORKER?"+option, time.Second); err == nil {
			t.Fatalf("accepted native credential override %s", strings.Split(option, "=")[0])
		}
	}
}

func TestNativeInventoryDoesNotHideCatalogFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	c := &connection{db: db, conn: conn, target: Target{Driver: driverOracle, Mode: driverOracle}, info: serverInfo{Schema: nativeTestDisposable}}
	denied := errors.New("catalog denied")
	mock.ExpectQuery("SELECT object_name, object_type FROM ALL_OBJECTS").WithArgs(nativeTestDisposable, nativeTestDisposable).WillReturnError(denied)
	if _, err := c.inventory(context.Background()); !errors.Is(err, denied) {
		t.Fatalf("catalog failure lost: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeIdentityRejectsModeSchemaProductAndAdmin(t *testing.T) {
	base := serverInfo{Product: driverOpenGauss, Version: "openGauss 6.0", Mode: modePG, Schema: nativeTestSchema, Database: "tests", User: "worker"}
	c := &connection{target: Target{Driver: driverOpenGauss, Mode: modePG, Schema: nativeTestSchema}}
	if err := c.validateIdentity(base, false); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*serverInfo){func(i *serverInfo) { i.Mode = modeA }, func(i *serverInfo) { i.Schema = "other" }, func(i *serverInfo) { i.Product = driverGaussDB }, func(i *serverInfo) { i.User = "SYSDBA" }, func(i *serverInfo) { i.Schema = "GOIMAGE" }} {
		info := base
		change(&info)
		if err := c.validateIdentity(info, false); err == nil {
			t.Fatal("unsafe identity accepted")
		}
	}
	if err := c.validateIdentity(base, true); err == nil {
		t.Fatal("administrator accepted")
	}
}

func TestNativeOracleIdentityUsesVersionAppropriateContainer(t *testing.T) {
	for _, tc := range []struct{ version, key string }{
		{"10.2.0.5.0", "DB_NAME"},
		{"11.2.0.4.0", "DB_NAME"},
		{"12.1.0.2.0", "CON_NAME"},
		{"19.3.0.0.0", "CON_NAME"},
	} {
		t.Run(tc.version, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			conn, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			c := &connection{conn: conn, target: Target{Driver: driverOracle, Mode: driverOracle, Schema: nativeTestDisposable}}
			mock.ExpectQuery(`SELECT BANNER FROM V\$VERSION`).WillReturnRows(sqlmock.NewRows([]string{"banner"}).AddRow("Oracle Database Enterprise Edition Release " + tc.version))
			mock.ExpectQuery("SELECT SYS_CONTEXT.*'" + tc.key + "'").WillReturnRows(sqlmock.NewRows([]string{"schema", "user", "database", "container", "server", "admin"}).AddRow(nativeTestDisposable, "worker", "testdb", "test-container", "test-host", 0))
			info, server, container, err := c.identity(context.Background())
			if err != nil || info.Database != "testdb" || server != "test-host" || container != "test-container" {
				t.Fatalf("identity=%+v server=%q container=%q: %v", info, server, container, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNativePeerRequiresSameServerDatabaseTenant(t *testing.T) {
	c := &connection{target: Target{Driver: driverOceanBase, Mode: modeMySQL}, info: serverInfo{Schema: modeA, Database: "db"}, server: "one", tenant: "1001"}
	other := *c
	other.info.Schema = "b"
	if err := c.peerCompatible(&other); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*connection){func(c *connection) { c.server = "two" }, func(c *connection) { c.tenant = "1002" }, func(c *connection) { c.info.Database = "other" }, func(c *connection) { c.info.Schema = modeA }} {
		peer := other
		change(&peer)
		if err := c.peerCompatible(&peer); err == nil {
			t.Fatal("incompatible peer accepted")
		}
	}
}

func TestNativeMigrationURLRejectsNativeCreationOptions(t *testing.T) {
	c := &connection{target: Target{Driver: driverOceanBase, Mode: modeMySQL}, dsn: "oceanbase://worker:p@host/tests?createDatabaseIfMissing=true"}
	if _, err := c.migrationURL("mp_history", time.Second); err == nil {
		t.Fatal("database creation accepted before preflight")
	}
}

func TestNativeDriverRejectsChangedSchemaBeforeConstruction(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	c := &connection{db: db, conn: conn, target: Target{Driver: driverOpenGauss, Mode: modePG, Schema: nativeTestSchema}, dsn: "opengauss://worker:p@host/tests"}
	mock.ExpectQuery("SELECT pg_catalog.version").WillReturnRows(sqlmock.NewRows([]string{"product", "database", "schema", "user", "mode", testServer, "admin"}).AddRow("openGauss 6.0", "tests", "production", "worker", "PG", "host:5432", false))
	if _, err := c.driver("mp_custom_history", time.Second); err == nil {
		t.Fatal("changed schema reached migration constructor")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeColumnPreservesExactMetadataAndNulls(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	c := &connection{conn: conn, target: Target{Driver: driverOracle}, info: serverInfo{Schema: nativeTestDisposable}}
	mock.ExpectQuery("SELECT DATA_TYPE").WithArgs(nativeTestDisposable, "MP_ITEMS", "AMOUNT").WillReturnRows(sqlmock.NewRows([]string{"type", "nullable", "length", "precision", "scale", "default"}).AddRow(typeNumber, "N", 22, 38, 9, nil))
	got, err := c.column(context.Background(), "mp_items", "amount")
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != typeNumber || got.Nullable || got.Precision != 38 || got.Scale != 9 || got.Length != 22 || got.Default != "" {
		t.Fatalf("metadata changed: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestNativeCharacterMetadataUsesLogicalLength(t *testing.T) {
	for _, driver := range []string{driverOracle, driverDM, driverOceanBase} {
		t.Run(driver, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			conn, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			c := &connection{conn: conn, target: Target{Driver: driver, Mode: driverOracle}, info: serverInfo{Schema: nativeTestDisposable}}
			mock.ExpectQuery("SELECT DATA_TYPE, NULLABLE, CASE WHEN DATA_TYPE IN .* THEN CHAR_LENGTH ELSE DATA_LENGTH END, DATA_PRECISION").WithArgs(nativeTestDisposable, "MP_ITEMS", "LABEL").WillReturnRows(sqlmock.NewRows([]string{"type", "nullable", "logical_length", "precision", "scale", "default"}).AddRow(typeVarchar2, "Y", 200, nil, nil, nil))
			got, err := c.column(context.Background(), "mp_items", "label")
			if err != nil {
				t.Fatal(err)
			}
			if got.Length != 200 || !got.Nullable || got.Precision != 0 || got.Scale != 0 {
				t.Fatalf("logical character metadata changed: %+v", got)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

const nativeTestSchema = "sandbox"

func TestNativeGlobalGrantsIdentifyAdministrator(t *testing.T) {
	for _, grant := range []string{"GRANT ALL PRIVILEGES ON *.* TO 'worker'@'%'", "GRANT SUPER, CREATE USER ON *.* TO 'worker'@'%'"} {
		if !globalAdminGrant(grant) {
			t.Fatal("global administrator grant accepted")
		}
	}
	for _, grant := range []string{"GRANT USAGE ON *.* TO 'worker'@'%'", "GRANT ALL PRIVILEGES ON `sandbox`.* TO 'worker'@'%'", "GRANT SELECT ON *.* TO 'observer'@'%'"} {
		if globalAdminGrant(grant) {
			t.Fatal("non-administrator grant rejected")
		}
	}
}

func TestNativeServerIdentityRejectsEmptyOrAmbiguousCatalog(t *testing.T) {
	for _, values := range [][]string{nil, {""}, {"1", "2"}, {"1001"}} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		conn, err := db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		rows := sqlmock.NewRows([]string{"value"})
		for _, value := range values {
			rows.AddRow(value)
		}
		mock.ExpectQuery("SELECT identity").WillReturnRows(rows)
		mock.ExpectClose()
		c := &connection{db: db, conn: conn}
		value, err := c.singleIdentityValue(context.Background(), "SELECT identity")
		wantError := len(values) != 1 || values[0] == ""
		if (err != nil) != wantError {
			t.Errorf("values=%v, got value=%q error=%v", values, value, err)
		}
		if err := c.close(); err != nil {
			t.Error(err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
	}
}

const nativeTestDisposable = "DISPOSABLE"
