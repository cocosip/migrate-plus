package gaussdb

import (
	"strings"
	"testing"
	"time"
)

func TestConfigDefaultsValidationAndCopy(t *testing.T) {
	config, err := prepareConfig(nil)
	if err != nil || config.MigrationsTable != "schema_migrations" || config.StatementTimeout != 0 {
		t.Fatalf("defaults = %+v, %v", config, err)
	}
	input := &Config{MigrationsTable: "History", StatementTimeout: time.Second}
	config, err = prepareConfig(input)
	if err != nil || config.MigrationsTable != "history" || input.MigrationsTable != "History" {
		t.Fatalf("normalization/copy = %+v, %+v, %v", config, input, err)
	}
	for _, name := range []string{"bad.name", `"history"`, "bad-name", "1bad", strings.Repeat("a", 64), "bad\x00name"} {
		if _, err := prepareConfig(&Config{MigrationsTable: name}); err == nil {
			t.Errorf("accepted table %q", name)
		}
	}
	if _, err := prepareConfig(&Config{StatementTimeout: -1}); err == nil {
		t.Fatal("accepted negative timeout")
	}
}

func TestURLUsesNativeConfigAndFiltersMigrationOptions(t *testing.T) {
	for _, authority := range []string{"localhost:5432", "[::1]:5432", "host1:5432,host2:5433"} {
		native, config, err := parseURL("gaussdb://u:p%40%3A%2F%2B%25%3F%26@" + authority + "/app?sslmode=disable&search_path=app&x-migrations-table=History&x-statement-timeout=25")
		if err != nil {
			t.Fatal(err)
		}
		if native.Password != "p@:/+%?&" || native.Database != testSchema || native.RuntimeParams["search_path"] != testSchema {
			t.Fatalf("native connection options changed: host=%s database=%s", native.Host, native.Database)
		}
		if config.MigrationsTable != "history" || config.StatementTimeout != 25*time.Millisecond {
			t.Fatalf("migration config = %+v", config)
		}
		for key := range native.RuntimeParams {
			if strings.HasPrefix(key, "x-") {
				t.Fatalf("migration option reached server: %s", key)
			}
		}
	}
}

func TestURLRejectsAmbiguityAndDoesNotExposePasswords(t *testing.T) {
	const secret = "DO_NOT_LEAK_42"
	base := "gaussdb://user:" + secret + "@localhost/app?sslmode=disable&"
	for _, query := range []string{
		"x-unknown=1", "x-statement-timeout=0", "x-statement-timeout=-1", "x-statement-timeout=9223372036854775807",
		"x-migrations-table=", "x-migrations-table=a&x-migrations-table=b", "X-Migrations-Table=a", "search_path=a&search_path=b",
		"x-migrations-table=%zz", "database=other", "dbname=other", "target_session_attrs=read-only",
		"target_session_attrs=standby", "default_transaction_read_only=on",
		"password=" + secret + "&default_query_exec_mode=invalid",
	} {
		_, _, err := parseURL(base + query)
		if err == nil {
			t.Errorf("accepted invalid connection option %q", query)
		} else if strings.Contains(err.Error(), secret) {
			t.Fatalf("password exposed: %v", err)
		}
	}
	for _, raw := range []string{"postgres://host/app", "gaussdb://u:%zz@host/app", base + "#fragment"} {
		if _, _, err := parseURL(raw); err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("unsafe invalid-URL error: %v", err)
		}
	}
}
