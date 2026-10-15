// Command migrate-plus runs migrations with the Oracle and DM drivers.
package main

import (
	"github.com/cocosip/migrate-plus/cli"
	_ "github.com/cocosip/migrate-plus/database/dm"
	_ "github.com/cocosip/migrate-plus/database/oracle"
)

// Version is set at build time using -ldflags.
var Version = "dev"

func main() {
	cli.Main(Version)
}
