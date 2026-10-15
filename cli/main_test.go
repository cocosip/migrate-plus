package cli

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Main uses process flags and exits, so each invocation needs a fresh process.
func TestCLIProcess(t *testing.T) {
	if os.Getenv("MIGRATE_PLUS_TEST_CLI") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"migrate-plus"}, os.Args[i+1:]...)
			flag.CommandLine = flag.NewFlagSet("migrate-plus", flag.ExitOnError)
			Main("test-build")
			os.Exit(0)
		}
	}
	t.Fatal("missing CLI argument separator")
}

func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, append([]string{"-test.run=^TestCLIProcess$", "--"}, args...)...)
	cmd.Env = append(os.Environ(), "MIGRATE_PLUS_TEST_CLI=1")
	output, err := cmd.CombinedOutput()
	return strings.ReplaceAll(string(output), "\r\n", "\n"), err
}

func TestBuildVersion(t *testing.T) {
	output, err := runCLI(t, "-version")
	if err != nil || output != "test-build\n" {
		t.Fatalf("build version: output=%q err=%v", output, err)
	}
}

func TestHelpUsesUpstreamCommands(t *testing.T) {
	output, err := runCLI(t, "-help")
	if err != nil {
		t.Fatalf("help: %v\n%s", err, output)
	}
	for _, text := range []string{"Usage: migrate-plus", "-digits N", "version      Print current migration version", "Source drivers: file", "Database drivers: \n"} {
		if !strings.Contains(output, text) {
			t.Errorf("help is missing %q:\n%s", text, output)
		}
	}
}

func TestDatabaseVersionRequiresConfiguration(t *testing.T) {
	output, err := runCLI(t, "version")
	if err == nil || strings.Contains(output, "test-build") {
		t.Fatalf("database version must require a database: output=%q err=%v", output, err)
	}
}

func TestCreateSequentialMigrations(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"init", "next"} {
		output, err := runCLI(t, "create", "-ext", "sql", "-dir", dir, "-seq", "-digits", "4", name)
		if err != nil {
			t.Fatalf("create %s: %v\n%s", name, err, output)
		}
	}
	for _, name := range []string{"0001_init.up.sql", "0001_init.down.sql", "0002_next.up.sql", "0002_next.down.sql"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("created migration %s: %v", name, err)
		}
	}
}

func TestCreateRejectsDuplicateVersion(t *testing.T) {
	dir := t.TempDir()
	args := []string{"create", "-ext", "sql", "-dir", dir, "-format", "2006", "init"}
	if output, err := runCLI(t, args...); err != nil {
		t.Fatalf("first create: %v\n%s", err, output)
	}
	output, err := runCLI(t, args...)
	if err == nil || !strings.Contains(output, "duplicate migration version") {
		t.Fatalf("duplicate create: output=%q err=%v", output, err)
	}
}
