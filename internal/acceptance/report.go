package acceptance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ExitCode returns 0 for complete success, 1 for incomplete/failure, 2 for no run.
func (r Report) ExitCode() int {
	if len(r.Targets) == 0 {
		return 2
	}
	if !r.selectionFinished() {
		return 1
	}
	executed := false
	for _, target := range r.Targets {
		if !target.Finished || len(target.Results) == 0 {
			return 1
		}
		for _, result := range target.Results {
			switch result.Status {
			case Pass:
				executed = true
			case NotApplicable:
			case Fail, Skipped, Blocked:
				return 1
			default:
				return 1
			}
		}
	}
	if !executed {
		return 2
	}
	return 0
}

func (r Report) selectionFinished() bool {
	unselected := map[string]bool{}
	finished := map[string]bool{}
	for _, entry := range r.Unselected {
		unselected[entry.Name] = true
	}
	for _, target := range r.Targets {
		finished[target.Name] = target.Finished
	}
	for _, entry := range r.Configured {
		if !unselected[entry.Name] && !finished[entry.Name] {
			return false
		}
	}
	return true
}

// Counts returns leaf totals with no suite/container double counting.
func (r Report) Counts() map[Status]int {
	result := map[Status]int{Pass: 0, Fail: 0, Skipped: 0, Blocked: 0, NotApplicable: 0}
	for _, t := range r.Targets {
		for _, entry := range t.Results {
			result[entry.Status]++
		}
	}
	return result
}

// Save atomically replaces a report, preserving the prior report on write failure.
func (r Report) Save(path string) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(struct {
		Report
		Counts   map[Status]int `json:"counts"`
		Complete bool           `json:"complete"`
	}{r, r.Counts(), r.ExitCode() == 0}, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".migration-report-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer func() { _ = temp.Close() }()
	defer func() { _ = os.Remove(name) }()
	if _, err = temp.Write(data); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	if err = temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// Native errors can contain credentials outside the DSN; redact known tokens too.
func redact(message string, secrets ...string) string {
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	return message
}
