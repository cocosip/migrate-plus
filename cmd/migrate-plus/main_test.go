package main

import (
	"testing"

	"github.com/golang-migrate/migrate/v4/database"
)

func TestOracleIsCompiledIntoCLI(t *testing.T) {
	for _, name := range database.List() {
		if name == "oracle" {
			return
		}
	}
	t.Fatal("CLI does not include the Oracle driver")
}
