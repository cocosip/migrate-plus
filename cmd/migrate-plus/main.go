// Command migrate-plus runs the official migration CLI.
package main

import "github.com/cocosip/migrate-plus/cli"

// Version is set at build time using -ldflags.
var Version = "dev"

func main() {
	cli.Main(Version)
}
