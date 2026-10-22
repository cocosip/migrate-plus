// Package oceanbase implements a golang-migrate driver for OceanBase MySQL and Oracle tenants.
package oceanbase

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	ob "github.com/cocosip/go-oceanbase"
)

// DefaultMigrationsTable is uppercased in Oracle mode and preserved in MySQL mode.
const DefaultMigrationsTable = "schema_migrations"

// Config configures migration state and operation deadlines.
type Config struct {
	MigrationsTable string
	// StatementTimeout bounds each operation, including an entire migration file.
	StatementTimeout time.Duration
}

var identifierPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

func prepareConfig(input *Config) (Config, error) {
	var config Config
	if input != nil {
		config = *input
	}
	if config.MigrationsTable == "" {
		config.MigrationsTable = DefaultMigrationsTable
	}
	// A common 64-byte limit keeps configuration valid in both modes.
	if len(config.MigrationsTable) > 64 || !identifierPattern.MatchString(config.MigrationsTable) {
		return Config{}, fmt.Errorf("oceanbase: migrations table must be an ASCII identifier of at most 64 bytes")
	}
	if config.StatementTimeout < 0 {
		return Config{}, fmt.Errorf("oceanbase: statement timeout cannot be negative")
	}
	return config, nil
}

func parseURL(raw string) (ob.Config, *Config, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "oceanbase" || u.Fragment != "" || u.Opaque != "" {
		return ob.Config{}, nil, fmt.Errorf("oceanbase: invalid database URL")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return ob.Config{}, nil, fmt.Errorf("oceanbase: invalid URL query")
	}
	config := Config{}
	for key, values := range query {
		if len(values) != 1 || values[0] == "" {
			return ob.Config{}, nil, fmt.Errorf("oceanbase: connection options require one nonempty value")
		}
		if !strings.HasPrefix(strings.ToLower(key), "x-") {
			continue
		}
		switch key {
		case "x-migrations-table":
			config.MigrationsTable = values[0]
		case "x-statement-timeout":
			millis, e := strconv.ParseInt(values[0], 10, 64)
			if e != nil || millis <= 0 || millis > int64((1<<63-1)/time.Millisecond) {
				return ob.Config{}, nil, fmt.Errorf("oceanbase: timeout must be positive milliseconds within time.Duration range")
			}
			config.StatementTimeout = time.Duration(millis) * time.Millisecond
		default:
			return ob.Config{}, nil, fmt.Errorf("oceanbase: unknown migration option")
		}
		query.Del(key)
	}
	normalized, err := prepareConfig(&config)
	if err != nil {
		return ob.Config{}, nil, err
	}
	if query.Has("allowMultiStatements") && query.Get("allowMultiStatements") != "true" {
		return ob.Config{}, nil, fmt.Errorf("oceanbase: migrations require allowMultiStatements=true")
	}
	query.Set("allowMultiStatements", "true")
	u.RawQuery = query.Encode()
	native, err := ob.ParseDSN(u.String())
	if err != nil {
		return ob.Config{}, nil, fmt.Errorf("oceanbase: invalid native connection configuration")
	}
	if native.CreateDatabaseIfMissing || native.Routing.Role == ob.RoleReadOnly {
		return ob.Config{}, nil, fmt.Errorf("oceanbase: migrations require an existing writable target")
	}
	return native, &normalized, nil
}
