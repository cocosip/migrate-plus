package cli

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
)

// Only the database boundary is replaced; tests run the real CLI and migrate.
type integerTestDatabase struct{ database.Driver }

func (d *integerTestDatabase) Open(string) (database.Driver, error) {
	fmt.Printf("initial-lock-timeout=%s\n", migrate.DefaultLockTimeout)
	return d, nil
}
func (*integerTestDatabase) Close() error                { return nil }
func (*integerTestDatabase) Lock() error                 { return nil }
func (*integerTestDatabase) Unlock() error               { return nil }
func (*integerTestDatabase) Version() (int, bool, error) { return 0, false, nil }
func (*integerTestDatabase) SetVersion(version int, dirty bool) error {
	fmt.Printf("version=%d dirty=%t\n", version, dirty)
	return nil
}

func TestCLIAppliesLockTimeoutBeforeOpeningDatabase(t *testing.T) {
	t.Setenv("MIGRATE_PLUS_TEST_DATABASE", "1")
	sourceURL := "file:" + filepath.ToSlash(t.TempDir())
	output, err := runCLI(t, "-lock-timeout", "2", "-source", sourceURL, "-database", "clitest://database", "version")
	if err != nil || !strings.Contains(output, "initial-lock-timeout=2s") {
		t.Fatalf("initialization timeout: output=%q err=%v", output, err)
	}
}

func TestCLIRejectsLockTimeoutOverflowBeforeOpeningDatabase(t *testing.T) {
	t.Setenv("MIGRATE_PLUS_TEST_DATABASE", "1")
	output, err := runCLI(t, "-lock-timeout", "18446744073709551615", "version")
	if err == nil || strings.Contains(output, "initial-lock-timeout=") {
		t.Fatalf("overflowing timeout reached database initialization: output=%q err=%v", output, err)
	}
}

func TestCLIRejectsIntegerOverflow(t *testing.T) {
	const invalidLimit = "can't read limit argument N"
	const invalidVersion = "can't read version argument V"
	const gotoCommand = "goto"
	t.Setenv("MIGRATE_PLUS_TEST_DATABASE", "1")
	intOverflow, uintOverflow := "9223372036854775808", "18446744073709551616"
	if strconv.IntSize == 32 {
		intOverflow, uintOverflow = "2147483648", "4294967296"
	}
	sourceURL := "file:" + filepath.ToSlash(t.TempDir())
	for _, tc := range []struct {
		command, value, message string
	}{
		{"force", intOverflow, invalidVersion},
		{gotoCommand, uintOverflow, invalidVersion},
		{gotoCommand, intOverflow, invalidVersion},
		{gotoCommand, strconv.FormatUint(^uint64(0), 10), invalidVersion},
		{"up", intOverflow, invalidLimit},
		{"down", intOverflow, invalidLimit},
		{"force", "-2", "argument V must be >= -1"},
		{"up", "-1", invalidLimit},
		{"down", "-1", invalidLimit},
	} {
		t.Run(tc.command+"/"+tc.value, func(t *testing.T) {
			output, err := runCLI(t, "-source", sourceURL, "-database", "clitest://database", tc.command, "--", tc.value)
			if err == nil || !strings.Contains(output, tc.message) {
				t.Fatalf("out-of-range argument accepted or misreported: output=%q err=%v", output, err)
			}
		})
	}
}

func TestForcePreservesBoundaryVersions(t *testing.T) {
	t.Setenv("MIGRATE_PLUS_TEST_DATABASE", "1")
	maxInt := "9223372036854775807"
	if strconv.IntSize == 32 {
		maxInt = "2147483647"
	}
	sourceURL := "file:" + filepath.ToSlash(t.TempDir())
	for _, version := range []string{"-1", "0", maxInt} {
		t.Run(version, func(t *testing.T) {
			output, err := runCLI(t, "-source", sourceURL, "-database", "clitest://database", "force", "--", version)
			if err != nil || !strings.Contains(output, "version="+version+" dirty=false") {
				t.Fatalf("force changed the version: output=%q err=%v", output, err)
			}
		})
	}
}

func TestDownLimitPreservesNativeIntMaximum(t *testing.T) {
	maxInt, want := "9223372036854775807", int64(9223372036854775807)
	if strconv.IntSize == 32 {
		maxInt, want = "2147483647", 2147483647
	}
	n, confirm, err := numDownMigrationsFromArgs(false, []string{maxInt})
	if err != nil || confirm || int64(n) != want {
		t.Fatalf("down limit = %d, %v, %v", n, confirm, err)
	}
}
