// Package opengauss implements a golang-migrate driver for openGauss using the official pure-Go client.
package opengauss

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	opq "gitcode.com/opengauss/openGauss-connector-go-pq"
)

// DefaultMigrationsTable is the default lowercase migration state table.
const DefaultMigrationsTable = "schema_migrations"

const settingOff = "off"

// Config configures a driver. A nil Config selects defaults.
type Config struct {
	MigrationsTable string
	// StatementTimeout bounds each operation, including an entire migration file.
	StatementTimeout time.Duration
}

type capabilities struct {
	mode            string
	standardStrings bool
}

func scriptCapabilities(mode, standard, encoding string) (capabilities, error) {
	if encoding != "UTF8" {
		return capabilities{}, fmt.Errorf("opengauss: SQL preflight requires client_encoding UTF8")
	}
	mode = strings.ToUpper(mode)
	if mode != "A" && mode != "PG" {
		return capabilities{}, fmt.Errorf("opengauss: unsupported sql_compatibility %q; native A and PG SQL framing is supported", mode)
	}
	if standard != "on" && standard != settingOff {
		return capabilities{}, fmt.Errorf("opengauss: unknown string escaping setting")
	}
	return capabilities{mode: mode, standardStrings: standard == "on"}, nil
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
	if len(config.MigrationsTable) > 63 || !identifierPattern.MatchString(config.MigrationsTable) {
		return Config{}, fmt.Errorf("opengauss: migrations table must be an ASCII identifier of at most 63 bytes")
	}
	if config.StatementTimeout < 0 {
		return Config{}, fmt.Errorf("opengauss: statement timeout cannot be negative")
	}
	config.MigrationsTable = strings.ToLower(config.MigrationsTable)
	return config, nil
}

func parseURL(raw string) (*opq.Config, *Config, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "opengauss" || u.Fragment != "" || u.Opaque != "" {
		return nil, nil, fmt.Errorf("opengauss: invalid database URL")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, nil, fmt.Errorf("opengauss: invalid URL query")
	}
	config := Config{}
	for key, values := range query {
		if len(values) != 1 {
			return nil, nil, fmt.Errorf("opengauss: duplicate connection option")
		}
		if strings.HasPrefix(strings.ToLower(key), "x-") {
			if err := migrationOption(&config, key, values[0]); err != nil {
				return nil, nil, err
			}
			query.Del(key)
		}
	}
	if err := validateNativeOptions(u, query); err != nil {
		return nil, nil, err
	}
	normalized, err := prepareConfig(&config)
	if err != nil {
		return nil, nil, err
	}
	u.RawQuery = query.Encode()
	native, err := opq.ParseConfig(u.String())
	if err != nil {
		// Native parse errors can retain query passwords even after userinfo redaction.
		return nil, nil, fmt.Errorf("opengauss: invalid native connection configuration")
	}
	return native, &normalized, nil
}

func migrationOption(config *Config, key, value string) error {
	if value == "" {
		return fmt.Errorf("opengauss: migration options require a nonempty value")
	}
	switch key {
	case "x-migrations-table":
		config.MigrationsTable = value
	case "x-statement-timeout":
		millis, err := strconv.ParseInt(value, 10, 64)
		if err != nil || millis <= 0 || millis > int64((1<<63-1)/time.Millisecond) {
			return fmt.Errorf("opengauss: statement timeout must be positive milliseconds within time.Duration range")
		}
		config.StatementTimeout = time.Duration(millis) * time.Millisecond
	default:
		return fmt.Errorf("opengauss: unknown migration option %q", key)
	}
	return nil
}

func validateNativeOptions(u *url.URL, query url.Values) error {
	path := strings.TrimLeft(u.Path, "/")
	for _, key := range []string{"database", "dbname"} {
		value, present := query[key]
		if present && (len(value) != 1 || (path != "" && value[0] != path)) {
			return fmt.Errorf("opengauss: conflicting database names")
		}
	}
	if query.Has("database") && query.Has("dbname") && query.Get("database") != query.Get("dbname") {
		return fmt.Errorf("opengauss: conflicting database aliases")
	}
	switch query.Get("target_session_attrs") {
	case "read-only", "standby", "prefer-standby":
		return fmt.Errorf("opengauss: migrations require a writable primary session")
	}
	if value := strings.ToLower(query.Get("default_transaction_read_only")); value != "" && value != settingOff && value != "false" && value != "0" {
		return fmt.Errorf("opengauss: migrations require a writable session")
	}
	return nil
}
