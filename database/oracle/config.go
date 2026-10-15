// Package oracle implements a golang-migrate database driver using go-ora v2.
package oracle

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
)

// DefaultMigrationsTable is normalized to uppercase, like an Oracle identifier.
const DefaultMigrationsTable = "schema_migrations"

// Config configures a driver. A nil Config uses the defaults.
type Config struct {
	MigrationsTable string
	// StatementTimeout bounds each database operation; zero disables the timeout.
	StatementTimeout time.Duration
}

type capabilities struct {
	identifierLimit int
}

var (
	versionPattern    = regexp.MustCompile(`^([0-9]+)\.([0-9]+)(\.[0-9]+){0,3}$`)
	identifierPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_$#]*$`)
)

func capabilitiesFor(server, compatible string) (capabilities, error) {
	limit := 128
	for _, value := range []string{server, compatible} {
		parts := versionPattern.FindStringSubmatch(strings.TrimSpace(value))
		if parts == nil {
			return capabilities{}, fmt.Errorf("oracle: invalid server or compatibility version %q", value)
		}
		major, majorErr := strconv.Atoi(parts[1])
		minor, minorErr := strconv.Atoi(parts[2])
		if majorErr != nil || minorErr != nil || major < 10 || (major == 10 && minor < 2) {
			return capabilities{}, fmt.Errorf("oracle: server and COMPATIBLE must be at least 10.2, got %q", value)
		}
		if major < 12 || (major == 12 && minor < 2) {
			limit = 30
		}
	}
	return capabilities{identifierLimit: limit}, nil
}

func prepareConfig(input *Config, caps capabilities) (Config, error) {
	var config Config
	if input != nil {
		config = *input
	}
	if config.MigrationsTable == "" {
		config.MigrationsTable = DefaultMigrationsTable
	}
	if !identifierPattern.MatchString(config.MigrationsTable) || len(config.MigrationsTable) > caps.identifierLimit {
		return Config{}, fmt.Errorf("oracle: migrations table must be an ASCII identifier of at most %d bytes", caps.identifierLimit)
	}
	if config.StatementTimeout < 0 {
		return Config{}, fmt.Errorf("oracle: statement timeout cannot be negative")
	}
	config.MigrationsTable = strings.ToUpper(config.MigrationsTable)
	return config, nil
}

func parseURL(raw string) (string, *Config, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "oracle" || u.Fragment != "" {
		// url.Parse errors include the original URL, which may contain a password.
		return "", nil, fmt.Errorf("oracle: invalid database URL")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", nil, fmt.Errorf("oracle: invalid URL query")
	}
	for key, values := range query {
		if !strings.HasPrefix(strings.ToLower(key), "x-") {
			continue
		}
		if key != "x-migrations-table" && key != "x-statement-timeout" {
			return "", nil, fmt.Errorf("oracle: unknown migration option %q", key)
		}
		if len(values) != 1 || values[0] == "" {
			return "", nil, fmt.Errorf("oracle: option %s must have exactly one nonempty value", key)
		}
	}
	config := &Config{MigrationsTable: query.Get("x-migrations-table")}
	if value := query.Get("x-statement-timeout"); value != "" {
		millis, err := strconv.ParseInt(value, 10, 64)
		if err != nil || millis <= 0 || millis > int64((1<<63-1)/time.Millisecond) {
			return "", nil, fmt.Errorf("oracle: x-statement-timeout must be positive milliseconds within time.Duration range")
		}
		config.StatementTimeout = time.Duration(millis) * time.Millisecond
	}
	if _, err := prepareConfig(config, capabilities{identifierLimit: 128}); err != nil {
		return "", nil, err
	}
	return migrate.FilterCustomQuery(u).String(), config, nil
}
