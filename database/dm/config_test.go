package dm

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCapabilitiesUseDMServerModeAndProductIdentity(t *testing.T) {
	for _, mode := range []int{0, 2, 4, 7} {
		caps, err := capabilitiesFor([]string{testProduct, "DB Version: 0x7000c", "Msg Version: 7.6.0.99"}, mode, 1, 0)
		if err != nil || caps.mode != mode || !caps.caseSensitive || caps.backslash {
			t.Fatalf("mode %d: %+v, %v", mode, caps, err)
		}
	}
	for _, banner := range []string{"DM Database Server 64 V7", "Msg Version: 8.1.4.200", "PostgreSQL 18", ""} {
		if _, err := capabilitiesFor([]string{banner}, 0, 0, 0); err == nil {
			t.Errorf("accepted product %q", banner)
		}
	}
	for _, mode := range []int{-1, 1, 3, 5, 6, 8} {
		if _, err := capabilitiesFor([]string{testProduct}, mode, 0, 0); err == nil {
			t.Errorf("accepted mode %d", mode)
		}
	}
	if _, err := capabilitiesFor([]string{testProduct}, 0, 2, 0); err == nil {
		t.Fatal("accepted unknown case behavior")
	}
}

func TestConfigDefaultsValidationAndCopy(t *testing.T) {
	got, err := prepareConfig(nil)
	if err != nil || got.MigrationsTable != "SCHEMA_MIGRATIONS" {
		t.Fatalf("default = %+v, %v", got, err)
	}
	input := &Config{MigrationsTable: historyInput, StatementTimeout: time.Second}
	got, err = prepareConfig(input)
	if err != nil || got.MigrationsTable != historyTable || input.MigrationsTable != historyInput {
		t.Fatalf("normalized %+v, input %+v: %v", got, input, err)
	}
	for _, name := range []string{"a.b", "\"a\"", "1history", "记录", strings.Repeat("X", 129)} {
		if _, err := prepareConfig(&Config{MigrationsTable: name}); err == nil {
			t.Errorf("accepted table %q", name)
		}
	}
	if _, err := prepareConfig(&Config{StatementTimeout: -1}); err == nil {
		t.Fatal("accepted negative timeout")
	}
}

func TestURLPreservesNativeCredentialsAndQueryBytes(t *testing.T) {
	for _, password := range []string{"p@ss", "p:ss", "p/ss", "p+ss", "p%ss", "p?ss", "p&ss", "p#ss"} {
		raw := "dm://user:" + password + "@localhost:5236/APP?schema=APP&dialName=custom+name&sslCertPath=C:%5Ccert&x-migrations-table=history&x-statement-timeout=25"
		dsn, config, err := parseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		want := "dm://user:" + password + "@localhost:5236/APP?schema=APP&dialName=custom+name&sslCertPath=C:%5Ccert&autoCommit=true&rwSeparate=0&doSwitch=0&driverReconnect=false&maxRows=0&enRsCache=false&escapeProcess=false"
		if dsn != want || config.MigrationsTable != historyTable || config.StatementTimeout != 25*time.Millisecond {
			t.Errorf("password %q: %q %+v", password, dsn, config)
		}
	}
}

func TestURLRejectsAmbiguousOrUnsafeOptionsWithoutLeakingCredentials(t *testing.T) {
	for _, suffix := range []string{
		"x-unknown=1", "X-migrations-table=x", "x-migrations-table=", "x-migrations-table=a&x-migrations-table=b",
		"x-statement-timeout=0", "x-statement-timeout=-1", "x-statement-timeout=9223372036854775807", "x-statement-timeout=wat",
		"autoCommit=false", "AUTOCOMMIT=0", "autoCommit=true&AUTOCOMMIT=true", "rwSeparate=1", "doSwitch=2", "driverReconnect=true",
		"schema=OTHER", "schema=APP&SCHEMA=OTHER", "x%2Dmigrations-table=x", "malformed",
	} {
		_, _, err := parseURL("dm://user:secret@localhost:5236/APP?" + suffix)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("option %q: %v", suffix, err)
		}
	}
	for _, raw := range []string{"oracle://u:secret@localhost", "dm://localhost", "dm://u:secret@", "dm://u:secret@host/path/extra"} {
		if _, _, err := parseURL(raw); err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("invalid URL: %v", err)
		}
	}
}

func TestURLSafeOptionsAreCanonicalized(t *testing.T) {
	dsn, _, err := parseURL("dm://u:p@host?AUTOCOMMIT=1&rwSeparate=0&doSwitch=0&driverReconnect=FALSE")
	if err != nil || !reflect.DeepEqual(dsn, "dm://u:p@host?autoCommit=true&rwSeparate=0&doSwitch=0&driverReconnect=false&maxRows=0&enRsCache=false&escapeProcess=false") {
		t.Fatalf("%q: %v", dsn, err)
	}
}

func TestURLRequiresUnmodifiedUnboundedFreshQueries(t *testing.T) {
	for _, option := range []string{"maxRows=1", "enRsCache=true", "escapeProcess=true", "MAXROWS=12", "ENRSCACHE=1"} {
		if _, _, err := parseURL("dm://u:p@host?" + option); err == nil {
			t.Errorf("accepted unsafe option %s", option)
		}
	}
	dsn, _, err := parseURL("dm://u:p@host?maxRows=0&enRsCache=0&escapeProcess=FALSE")
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range []string{"maxRows=0", "enRsCache=false", "escapeProcess=false"} {
		if !strings.Contains(dsn, setting) {
			t.Errorf("missing invariant %s", setting)
		}
	}
}
