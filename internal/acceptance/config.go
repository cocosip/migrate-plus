package acceptance

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

type configuration struct {
	Targets []Target `json:"targets"`
}

var safeName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

var modes = map[string][]string{
	driverOracle: {driverOracle}, driverDM: {modeNative, driverOracle, modeMySQL, modePG},
	driverGaussDB: {"a", modePG}, driverOpenGauss: {"a", modePG}, driverOceanBase: {modeMySQL, driverOracle},
}

func readConfig(reader io.Reader) (configuration, error) {
	data, err := io.ReadAll(io.LimitReader(reader, 1024*1024+1))
	if err != nil {
		return configuration{}, fmt.Errorf("read configuration: %w", err)
	}
	if len(data) > 1024*1024 {
		return configuration{}, fmt.Errorf("configuration exceeds 1 MiB")
	}
	if err := uniqueJSON(json.NewDecoder(strings.NewReader(string(data)))); err != nil {
		return configuration{}, fmt.Errorf("invalid or duplicate JSON fields")
	}
	var cfg configuration
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("invalid configuration fields")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return cfg, fmt.Errorf("configuration must contain one JSON value")
	}
	if len(cfg.Targets) == 0 {
		return cfg, fmt.Errorf("configuration has no targets")
	}
	names := map[string]Target{}
	for i := range cfg.Targets {
		t := &cfg.Targets[i]
		if err := t.validate(); err != nil {
			return cfg, fmt.Errorf("target %d: %w", i+1, err)
		}
		if _, ok := names[t.Name]; ok {
			return cfg, fmt.Errorf("duplicate target name")
		}
		names[t.Name] = *t
	}
	for _, t := range cfg.Targets {
		if t.OtherTarget == "" {
			continue
		}
		peer, ok := names[t.OtherTarget]
		if !ok || peer.Name == t.Name || peer.Driver != t.Driver || peer.Mode != t.Mode || strings.EqualFold(peer.Schema, t.Schema) {
			return cfg, fmt.Errorf("invalid other_target pairing")
		}
	}
	return cfg, nil
}

func uniqueJSON(d *json.Decoder) error {
	tok, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for d.More() {
		if delim == '{' {
			key, e := d.Token()
			if e != nil {
				return e
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("duplicate field")
			}
			seen[name] = true
		}
		if err := uniqueJSON(d); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

func (t *Target) validate() error {
	if !safeName.MatchString(strings.ReplaceAll(t.Name, "-", "_")) || len(t.Name) > 64 {
		return fmt.Errorf("invalid target name")
	}
	valid := false
	for _, mode := range modes[t.Driver] {
		valid = valid || mode == t.Mode
	}
	if !valid {
		return fmt.Errorf("unsupported driver/mode")
	}
	if !safeName.MatchString(t.Schema) || len(t.Schema) > 63 || forbiddenSchema(t.Schema) {
		return fmt.Errorf("require a non-system disposable schema; GOIMAGE is forbidden")
	}
	if (t.DSN == "") == (t.DSNEnv == "") {
		return fmt.Errorf("provide exactly one of dsn or dsn_env")
	}
	if t.TimeoutSeconds == 0 {
		t.TimeoutSeconds = 1800
	}
	if t.StatementTimeoutSeconds == 0 {
		t.StatementTimeoutSeconds = 15
	}
	if t.TimeoutSeconds < 1 || t.TimeoutSeconds > 86400 || t.StatementTimeoutSeconds < 1 || t.StatementTimeoutSeconds > 600 || t.StatementTimeoutSeconds >= t.TimeoutSeconds {
		return fmt.Errorf("invalid timeout bounds")
	}
	return nil
}

func forbiddenSchema(value string) bool {
	upper := strings.ToUpper(value)
	switch upper {
	case "GOIMAGE", "SYS", "SYSTEM", "SYSDBA", "SYSAUDITOR", "SYSSSO", "SYSCAT", "PUBLIC", "MYSQL", "INFORMATION_SCHEMA", "PERFORMANCE_SCHEMA", "OCEANBASE":
		return true
	}
	return strings.HasPrefix(upper, "PG_") || strings.HasPrefix(upper, "SYS$")
}

func (c configuration) selectTargets(name string) ([]Target, error) {
	if name == "" {
		return c.Targets, nil
	}
	for _, t := range c.Targets {
		if t.Name == name {
			return []Target{t}, nil
		}
	}
	return nil, fmt.Errorf("unknown selected target")
}

func (t Target) resolveDSN() (string, error) {
	value := t.DSN
	if t.DSNEnv != "" {
		value = os.Getenv(t.DSNEnv)
	}
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("selected target connection is not configured")
	}
	return value, nil
}

func (t Target) statementTimeout() time.Duration {
	return time.Duration(t.StatementTimeoutSeconds) * time.Second
}
