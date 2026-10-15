// Command migrate-plus runs migrations with the Oracle driver.
package main

import (
	"github.com/cocosip/migrate-plus/cli"
	_ "github.com/cocosip/migrate-plus/database/oracle"
)

// Version is set at build time using -ldflags.
var Version = "dev"

func main() {
	cli.Main(Version)
}
