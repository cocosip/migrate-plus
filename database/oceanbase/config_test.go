package oceanbase

import (
	"strings"
	"testing"
	"time"

	ob "github.com/cocosip/go-oceanbase"
)

func TestParseURL(t *testing.T) {
	native, config, err := parseURL("oceanbase://app%40tenant:p%40ss@localhost:2881/demo?mode=mysql&x-migrations-table=history&x-statement-timeout=250")
	if err != nil {
		t.Fatal(err)
	}
	if native.User != "app@tenant" || native.Password != "p@ss" || native.Mode != ob.ModeMySQL || !native.AllowMultiStatements || config.MigrationsTable != "history" || config.StatementTimeout != 250*time.Millisecond {
		t.Fatalf("unexpected native/config: user=%s mode=%v config=%+v", native.User, native.Mode, config)
	}
}

func TestRejectInvalidConfiguration(t *testing.T) {
	for _, suffix := range []string{"x-statement-timeout=0", "x-statement-timeout=-1", "x-statement-timeout=9223372036854775807", "x-statement-timeout=1&x-statement-timeout=2", "x-migrations-table=a.b", "x-migrations-table=", "x-unknown=1", "allowMultiStatements=false", "createDatabaseIfMissing=true", "role=read-only", "bogus=1"} {
		t.Run(suffix, func(t *testing.T) {
			_, _, err := parseURL("oceanbase://app:secret@localhost/demo?" + suffix)
			if err == nil {
				t.Fatal("accepted unsafe URL")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("error retained credentials")
			}
		})
	}
	for _, raw := range []string{"oracle://app:secret@localhost", "oceanbase://app:secret@localhost/%zz", "oceanbase://app:secret@localhost/demo#fragment"} {
		if _, _, err := parseURL(raw); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("invalid URL error: %v", err)
		}
	}
	if _, err := prepareConfig(&Config{StatementTimeout: -time.Second}); err == nil {
		t.Fatal("negative timeout accepted")
	}
}
