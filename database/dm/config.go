// Package dm implements a golang-migrate database driver using the native DM client.
package dm

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultMigrationsTable is normalized to uppercase in driver SQL.
const DefaultMigrationsTable = "schema_migrations"

// Config configures a driver. A nil Config selects the defaults.
type Config struct {
	MigrationsTable string
	// StatementTimeout bounds each database operation; zero disables the timeout.
	StatementTimeout time.Duration
}

type capabilities struct {
	mode          int
	caseSensitive bool
	backslash     bool
}

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_$#]*$`)
	dm8Pattern        = regexp.MustCompile(`(?i)\bDM(?:8\b|\s+Database\s+Server\b[^\r\n]*(?:\bV8\b|\b8\.[0-9]))`)
)

func capabilitiesFor(banners []string, mode, caseSensitive, backslash int) (capabilities, error) {
	product := false
	for _, banner := range banners {
		product = product || dm8Pattern.MatchString(banner)
	}
	if !product {
		return capabilities{}, fmt.Errorf("dm: cannot identify a DM8 product version from V$VERSION")
	}
	switch mode {
	case 0, 2, 4, 7:
	default:
		return capabilities{}, fmt.Errorf("dm: unsupported server COMPATIBLE_MODE %d (supported: 0, 2, 4, 7)", mode)
	}
	if (caseSensitive != 0 && caseSensitive != 1) || (backslash != 0 && backslash != 1) {
		return capabilities{}, fmt.Errorf("dm: unknown identifier case or backslash escape behavior")
	}
	return capabilities{mode: mode, caseSensitive: caseSensitive == 1, backslash: backslash == 1}, nil
}

func prepareConfig(input *Config) (Config, error) {
	var config Config
	if input != nil {
		config = *input
	}
	if config.MigrationsTable == "" {
		config.MigrationsTable = DefaultMigrationsTable
	}
	if len(config.MigrationsTable) > 128 || !identifierPattern.MatchString(config.MigrationsTable) {
		return Config{}, fmt.Errorf("dm: migrations table must be an ASCII identifier of at most 128 bytes")
	}
	if config.StatementTimeout < 0 {
		return Config{}, fmt.Errorf("dm: statement timeout cannot be negative")
	}
	config.MigrationsTable = strings.ToUpper(config.MigrationsTable)
	return config, nil
}

// parseURL follows the native client's raw DSN rules, not net/url query decoding.
func parseURL(raw string) (string, *Config, error) {
	if !strings.HasPrefix(raw, "dm://") {
		return "", nil, fmt.Errorf("dm: invalid database URL")
	}
	base, query := raw, ""
	if i := strings.LastIndexByte(raw, '?'); i >= 0 && strings.Contains(raw[:i], "@") {
		base, query = raw[:i], raw[i+1:]
	}
	at := strings.LastIndexByte(base, '@')
	if at < 5 || at == len(base)-1 || !strings.Contains(base[5:at], ":") {
		return "", nil, fmt.Errorf("dm: invalid database URL")
	}
	host, pathSchema, _ := strings.Cut(base[at+1:], "/")
	if host == "" || strings.Contains(pathSchema, "/") {
		return "", nil, fmt.Errorf("dm: invalid host or schema path")
	}
	config := Config{}
	seen := make(map[string]bool)
	var native []string
	for _, item := range strings.Split(query, "&") {
		if item == "" {
			continue
		}
		key, value, ok := strings.Cut(item, "=")
		lower := strings.ToLower(key)
		if !ok || key == "" || strings.ContainsAny(key, "%?\r\n") || seen[lower] {
			return "", nil, fmt.Errorf("dm: malformed or duplicate connection option")
		}
		seen[lower] = true
		if strings.HasPrefix(lower, "x-") {
			if err := migrationOption(&config, key, value); err != nil {
				return "", nil, err
			}
			continue
		}
		if lower == "schema" && pathSchema != "" && value != pathSchema {
			return "", nil, fmt.Errorf("dm: conflicting path and query schemas")
		}
		if required, guarded := migrationClientSettings[lower]; guarded {
			if !clientSettingMatches(lower, value, required) {
				return "", nil, fmt.Errorf("dm: unsafe connection option %s for session-locked migrations", lower)
			}
			continue
		}
		native = append(native, item)
	}
	normalized, err := prepareConfig(&config)
	if err != nil {
		return "", nil, err
	}
	native = append(native, "autoCommit=true", "rwSeparate=0", "doSwitch=0", "driverReconnect=false", "maxRows=0", "enRsCache=false", "escapeProcess=false")
	return base + "?" + strings.Join(native, "&"), &normalized, nil
}

const clientFalse = "false"

var migrationClientSettings = map[string]string{
	"autocommit": "true", "rwseparate": "0", "doswitch": "0", "driverreconnect": clientFalse,
	"maxrows": "0", "enrscache": clientFalse, "escapeprocess": clientFalse,
}

func clientSettingMatches(key, value, required string) bool {
	if key == "autocommit" || key == "driverreconnect" || key == "enrscache" || key == "escapeprocess" {
		got, err := strconv.ParseBool(value)
		return err == nil && got == (required == "true")
	}
	return value == required
}

func migrationOption(config *Config, key, value string) error {
	if value == "" {
		return fmt.Errorf("dm: migration options must be nonempty")
	}
	switch key {
	case "x-migrations-table":
		config.MigrationsTable = value
	case "x-statement-timeout":
		millis, err := strconv.ParseInt(value, 10, 64)
		if err != nil || millis <= 0 || millis > int64((1<<63-1)/time.Millisecond) {
			return fmt.Errorf("dm: x-statement-timeout must be positive milliseconds within time.Duration range")
		}
		config.StatementTimeout = time.Duration(millis) * time.Millisecond
	default:
		return fmt.Errorf("dm: unknown migration option")
	}
	return nil
}
