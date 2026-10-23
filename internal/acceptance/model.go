// Package acceptance runs destructive migration regression in disposable schemas.
package acceptance

import (
	"io/fs"
	"time"
)

// Target selects one explicitly disposable server schema and compatibility mode.
type Target struct {
	Name                    string `json:"name"`
	Driver                  string `json:"driver"`
	Mode                    string `json:"mode"`
	Schema                  string `json:"schema"`
	DSN                     string `json:"dsn,omitempty"`
	DSNEnv                  string `json:"dsn_env,omitempty"`
	OtherTarget             string `json:"other_target,omitempty"`
	TimeoutSeconds          int    `json:"timeout_seconds,omitempty"`
	StatementTimeoutSeconds int    `json:"statement_timeout_seconds,omitempty"`
}

type serverInfo struct {
	Fingerprint string `json:"server_identity,omitempty"`
	Product     string `json:"product"`
	Version     string `json:"version"`
	Mode        string `json:"mode"`
	Schema      string `json:"schema"`
	Database    string `json:"database,omitempty"`
	User        string `json:"user,omitempty"`
}

type object struct{ Name, Kind string }

type profile struct {
	ID, Driver, Mode, Family string
	FS                       fs.FS
	Types                    []typeCase
}

type typeCase struct {
	MetadataTypes                                                   []string
	ID, Name, DDL, Insert, Query, Expected, Update, Updated, Reason string
	Setup, Teardown                                                 string
}

// Status describes one leaf assertion; unavailable coverage never counts as pass.
type Status string

// Leaf result statuses are deliberately distinct from suite completeness.
const (
	Pass          Status = "pass"
	Fail          Status = "fail"
	Skipped       Status = "skipped"
	Blocked       Status = "blocked"
	NotApplicable Status = "not_applicable"
)

const (
	typeBigInt      = "BIGINT"
	typeInteger     = "INTEGER"
	typeNumber19    = "NUMBER(19)"
	typeClob        = "CLOB"
	nullValue       = "<NULL>"
	constructorURL  = "url"
	typeText        = "text"
	typeNVarchar2   = "NVARCHAR2"
	typeNumber      = "NUMBER"
	typeVarchar2    = "VARCHAR2"
	typeDouble      = "DOUBLE"
	typeReal        = "REAL"
	typeBoolean     = "BOOLEAN"
	typeFloat       = "FLOAT"
	typeTimestamp   = "TIMESTAMP"
	typeDate        = "DATE"
	sqlNull         = "NULL"
	errorUnique     = "unique"
	errorNull       = "null"
	errorCheck      = "check"
	errorForeignKey = "foreign-key"
	createUpFile    = "1_create.up.sql"
	createDownFile  = "1_create.down.sql"
)

// Result is one independently counted regression case.
type Result struct {
	ID       string        `json:"id"`
	Status   Status        `json:"status"`
	Detail   string        `json:"detail,omitempty"`
	Duration time.Duration `json:"duration_ns"`
}

// TargetReport records server identity, coverage and cleanup observations.
type TargetReport struct {
	Finished bool       `json:"finished"`
	Name     string     `json:"name"`
	Profile  string     `json:"profile"`
	Server   serverInfo `json:"server"`
	Results  []Result   `json:"results"`
}

// Report is the incremental, credential-free result of selected targets.
type Report struct {
	Version    string         `json:"tool_version"`
	Started    time.Time      `json:"started"`
	Configured []Selection    `json:"configured_targets,omitempty"`
	Unselected []Selection    `json:"unselected_targets,omitempty"`
	Targets    []TargetReport `json:"targets"`
}

// Selection identifies configured coverage without including native credentials.
type Selection struct {
	Name    string `json:"name"`
	Profile string `json:"profile"`
}
