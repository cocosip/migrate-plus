// Command migrate-test exercises migration drivers against disposable servers.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/cocosip/migrate-plus/internal/acceptance"
)

var version = "dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	os.Exit(acceptance.Main(ctx, os.Args[1:], os.Stdout, version))
}
