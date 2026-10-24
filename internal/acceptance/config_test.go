package acceptance

import (
	"strings"
	"testing"
)

func TestConfigurationRejectsUnsafeAndAmbiguousTargets(t *testing.T) {
	for _, raw := range []string{
		`{"targets":[{"name":"a","driver":"dm","mode":"oracle","schema":"GOIMAGE","dsn":"dm://u:p@host"}]}`,
		`{"targets":[{"name":"a","driver":"dm","mode":"pg","schema":"TEST","dsn":"x","dsn_env":"X"}]}`,
		`{"targets":[{"name":"a","driver":"dm","mode":"pg","schema":"TEST","dsn_env":"X","typo":1}]}`,
		`{"targets":[{"name":"a","name":"b","driver":"dm","mode":"pg","schema":"TEST","dsn_env":"X"}]}`,
	} {
		if _, err := readConfig(strings.NewReader(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestConfigurationSelectsTargetAndEnvironmentWithoutLeaking(t *testing.T) {
	cfg, err := readConfig(strings.NewReader(`{"targets":[{"name":"dm-pg","driver":"dm","mode":"pg","schema":"MP_TEST","dsn_env":"MP_DSN"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	selected, err := cfg.selectTargets("dm-pg")
	if err != nil || len(selected) != 1 {
		t.Fatalf("selection = %v, %v", selected, err)
	}
	t.Setenv("MP_DSN", "dm://u:p@host")
	if got, err := selected[0].resolveDSN(); err != nil || got != "dm://u:p@host" {
		t.Fatalf("DSN = %q %v", got, err)
	}
	if _, err := cfg.selectTargets("missing"); err == nil {
		t.Fatal("missing selection accepted")
	}
}

func TestConfigurationReportsUnderlyingJSONFailure(t *testing.T) {
	if _, err := readConfig(strings.NewReader(`{"targets":`)); err == nil ||
		!strings.Contains(err.Error(), "invalid or duplicate JSON fields") || !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("duplicate-check detail lost: %v", err)
	}
	if _, err := readConfig(strings.NewReader(`{"targets":[],"extra":1}`)); err == nil ||
		!strings.Contains(err.Error(), "invalid configuration fields") || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("decode detail lost: %v", err)
	}
}

func TestReportCannotPassIncompleteCoverage(t *testing.T) {
	r := Report{Targets: []TargetReport{{Name: "a", Finished: true, Results: []Result{{ID: "ok", Status: Pass}, {ID: "needs-peer", Status: Skipped}}}}}
	if r.ExitCode() != 1 {
		t.Fatal("incomplete report passed")
	}
	r.Targets[0].Results[1].Status = NotApplicable
	if r.ExitCode() != 0 {
		t.Fatal("not-applicable counted as missing")
	}
	if (Report{}).ExitCode() != 2 {
		t.Fatal("empty report passed")
	}
}
