package oracle

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	testVersion19    = "19.0.0"
	testVersion11    = "11.2.0"
	testVersion10Old = "10.1.0"
)

func TestCapabilitiesRespectServerAndCompatibility(t *testing.T) {
	for _, tc := range []struct {
		server, compatible string
		limit              int
	}{
		{"10.2.0.5.0", "10.2.0", 30},
		{"11.2.0.4.0", testVersion11, 30},
		{"12.1.0.2.0", "12.1.0", 30},
		{"12.2.0.1.0", "12.2.0", 128},
		{"18.0.0.0.0", "12.1.0", 30},
		{"19.0.0.0.0", testVersion11, 30},
		{"19.0.0.0.0", testVersion19, 128},
		{"21.0.0.0.0", "21.0.0", 128},
		{"23.0.0.0.0", "23.0.0", 128},
		{"11.2.0.4.0", testVersion19, 30},
	} {
		t.Run(tc.server+"/"+tc.compatible, func(t *testing.T) {
			caps, err := capabilitiesFor(tc.server, tc.compatible)
			if err != nil || caps.identifierLimit != tc.limit {
				t.Fatalf("capabilities = %+v, %v; want identifier limit %d", caps, err, tc.limit)
			}
			_, err = prepareConfig(&Config{MigrationsTable: "M" + strings.Repeat("x", 30)}, caps)
			if (err == nil) != (tc.limit == 128) {
				t.Fatalf("31-byte table name accepted incorrectly: %v", err)
			}
		})
	}
}

func TestCapabilitiesRejectUnsupportedAndUnknownVersions(t *testing.T) {
	for _, pair := range [][2]string{
		{testVersion10Old, testVersion10Old}, {"9.2.0", "9.2.0"}, {testVersion19, testVersion10Old},
		{"unknown", testVersion19}, {testVersion19, ""}, {"19", testVersion19},
	} {
		if _, err := capabilitiesFor(pair[0], pair[1]); err == nil {
			t.Errorf("accepted server/compatibility %q", pair)
		}
	}
}

func TestParseURLPreservesNativeOptionsAndCredentials(t *testing.T) {
	dsn, config, err := parseURL("oracle://user:p%40ss%2Fword@localhost:1521/service?SSL=true&server=backup:1521&x-migrations-table=history&x-statement-timeout=1500")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	password, _ := u.User.Password()
	if password != "p@ss/word" || u.Query().Get("SSL") != "true" || u.Query().Get("server") != "backup:1521" || strings.Contains(dsn, "x-") {
		t.Fatalf("native connection options changed: %s", u.Redacted())
	}
	if config.MigrationsTable != "history" || config.StatementTimeout != 1500*time.Millisecond {
		t.Fatalf("wrong migration config: %+v", config)
	}
}

func TestParseURLRejectsInvalidOptionsWithoutLeakingPassword(t *testing.T) {
	for _, raw := range []string{
		"oracle://user:secret%xx@localhost/service", "postgres://user:secret@localhost/service",
		"oracle://user:secret@localhost/service?x-statement-timeout=-1",
		"oracle://user:secret@localhost/service?x-statement-timeout=0",
		"oracle://user:secret@localhost/service?x-statement-timeout=9223372036854775807",
		"oracle://user:secret@localhost/service?x-statement-timeout=abc",
		"oracle://user:secret@localhost/service?x-typo=true",
		"oracle://user:secret@localhost/service?x-migrations-table=a&x-migrations-table=b",
		"oracle://user:secret@localhost/service?x-migrations-table=%xx",
	} {
		_, _, err := parseURL(raw)
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("expected safe validation error for URL, got %v", err)
		}
	}
}

func TestPrepareConfigValidatesAndCopies(t *testing.T) {
	caps := capabilities{identifierLimit: 128}
	input := &Config{MigrationsTable: "migration_history"}
	got, err := prepareConfig(input, caps)
	if err != nil || got.MigrationsTable != "MIGRATION_HISTORY" || input.MigrationsTable != "migration_history" {
		t.Fatalf("config normalization: %+v, %v", got, err)
	}
	got, err = prepareConfig(nil, caps)
	if err != nil || got.MigrationsTable != "SCHEMA_MIGRATIONS" {
		t.Fatalf("default config: %+v, %v", got, err)
	}
	for _, name := range []string{"A.B", `"table"`, "a;DROP TABLE users", "9table", "a b", "表", "A" + strings.Repeat("x", 128)} {
		if _, err := prepareConfig(&Config{MigrationsTable: name}, caps); err == nil {
			t.Errorf("accepted invalid name %q", name)
		}
	}
	if _, err := prepareConfig(&Config{StatementTimeout: -time.Second}, caps); err == nil {
		t.Error("accepted negative timeout")
	}
}
