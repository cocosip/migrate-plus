package acceptance

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testListFlag   = "-list"
	testConfigFlag = "-config"
	testServer     = "server"
)

func TestCommandListsWithoutDatabaseAndRejectsInvalidConfiguration(t *testing.T) {
	var out bytes.Buffer
	if code := Main(context.Background(), []string{testListFlag}, &out, "test"); code != 0 || !strings.Contains(out.String(), "workflow.run") || !strings.Contains(out.String(), "dm-pg") {
		t.Fatalf("list code/output=%d %s", code, out.String())
	}
	out.Reset()
	if code := Main(context.Background(), []string{"-unknown"}, &out, "test"); code != 2 {
		t.Fatalf("invalid option exit=%d", code)
	}
}

func TestCommandMissingDSNProducesIncompleteReport(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "targets.json")
	report := filepath.Join(dir, "report.json")
	raw := `{"targets":[{"name":"dm-pg","driver":"dm","mode":"pg","schema":"MP_TEST","dsn_env":"MIGRATE_ACCEPTANCE_UNCONFIGURED"}]}`
	if err := os.WriteFile(config, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MIGRATE_ACCEPTANCE_UNCONFIGURED", "")
	var out bytes.Buffer
	if code := Main(context.Background(), []string{testConfigFlag, config, "-report", report}, &out, "test"); code != 1 {
		t.Fatalf("missing target exit=%d output=%s", code, out.String())
	}
	data, err := os.ReadFile(report)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"complete": false`)) {
		t.Fatalf("report=%s", data)
	}
}

func TestReportWriteFailureIsNotSuccess(t *testing.T) {
	r := Report{Targets: []TargetReport{{Name: "a", Results: []Result{{ID: "ok", Status: Pass}}}}}
	if err := r.Save(filepath.Join(t.TempDir(), "missing", "report.json")); err == nil {
		t.Fatal("write failure swallowed")
	}
}

func TestPassingPartialReportNeverClaimsCompletion(t *testing.T) {
	r := Report{Targets: []TargetReport{{Name: "partial", Results: []Result{{ID: "preflight", Status: Pass}}}}}
	if r.ExitCode() != 1 {
		t.Fatal("passing prefix claimed success")
	}
	path := filepath.Join(t.TempDir(), "partial.json")
	if err := r.Save(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"complete": false`)) {
		t.Fatal("persisted passing prefix claimed completion")
	}
}

func TestReportWaitsForEverySelectedTarget(t *testing.T) {
	r := Report{Configured: []Selection{{Name: "selected_one"}, {Name: "second"}}, Targets: []TargetReport{{Name: "selected_one", Finished: true, Results: []Result{{ID: "done", Status: Pass}}}}}
	if r.ExitCode() != 1 {
		t.Fatal("first completed target claimed the whole selection passed")
	}
	r.Unselected = []Selection{{Name: "second"}}
	if r.ExitCode() != 0 {
		t.Fatal("unselected target blocked complete selected coverage")
	}
}

func TestManifestHasNoDuplicateIDs(t *testing.T) {
	if err := VerifyManifest(); err != nil {
		t.Fatal(err)
	}
}

func TestListHonorsSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "targets.json")
	data := `{"targets":[{"name":"one","driver":"dm","mode":"pg","schema":"MP_ONE","dsn_env":"ONE"},{"name":"two","driver":"oracle","mode":"oracle","schema":"MP_TWO","dsn_env":"TWO"}]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := Main(context.Background(), []string{testConfigFlag, path, "-target", "one", testListFlag}, &out, "test"); code != 0 || strings.Contains(out.String(), "oracle-oracle") || !strings.Contains(out.String(), "dm-pg") {
		t.Fatalf("selected list=%d %s", code, out.String())
	}
	if code := Main(context.Background(), []string{testConfigFlag, path, "-target", "absent", testListFlag}, &out, "test"); code != 2 {
		t.Fatalf("unknown list target exit=%d", code)
	}
}
