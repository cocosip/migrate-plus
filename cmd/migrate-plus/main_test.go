package main

import (
	"flag"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4/database"
)

func TestDMIsCompiledIntoCLI(t *testing.T) {
	if !slices.Contains(database.List(), "dm") {
		t.Fatal("CLI does not include the DM driver")
	}
}

func TestGaussDBIsCompiledIntoCLI(t *testing.T) {
	if !slices.Contains(database.List(), "gaussdb") {
		t.Fatal("CLI does not include the GaussDB driver")
	}
}

func TestOpenGaussIsCompiledIntoCLI(t *testing.T) {
	if !slices.Contains(database.List(), "opengauss") {
		t.Fatal("CLI does not include the openGauss driver")
	}
}

func TestComposedCLIProcess(_ *testing.T) {
	if os.Getenv("MIGRATE_PLUS_COMPOSED_CLI_TEST") != "1" {
		return
	}
	os.Args = []string{"migrate-plus", "-help"}
	flag.CommandLine = flag.NewFlagSet("migrate-plus", flag.ExitOnError)
	main()
}

func TestComposedHelpListsDrivers(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestComposedCLIProcess$")
	cmd.Env = append(os.Environ(), "MIGRATE_PLUS_COMPOSED_CLI_TEST=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("help: %v\n%s", err, output)
	}
	for _, name := range []string{"dm", "oracle", "gaussdb", "opengauss"} {
		if !strings.Contains(string(output), name) {
			t.Errorf("help missing %s: %s", name, output)
		}
	}
}

func TestOracleIsCompiledIntoCLI(t *testing.T) {
	for _, name := range database.List() {
		if name == "oracle" {
			return
		}
	}
	t.Fatal("CLI does not include the Oracle driver")
}
