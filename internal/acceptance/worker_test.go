package acceptance

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The real child process entry is replaced only inside this package's test binary.
func TestMain(m *testing.M) {
	if os.Getenv("MIGRATE_ACCEPTANCE_WORKER_HELPER") == "1" && len(os.Args) > 1 && os.Args[1] == "-worker" {
		_, _ = fmt.Fprintln(os.Stdout, "helper native error secret-password")
		time.Sleep(30 * time.Second)
		os.Exit(3)
	}
	os.Exit(m.Run())
}

func TestWorkerDeadlineIsBoundedRedactedAndDoesNotCleanupWithoutOwnership(t *testing.T) {
	t.Setenv("MIGRATE_ACCEPTANCE_WORKER_HELPER", "1")
	target := Target{Name: "dm-pg", Driver: driverDM, Mode: modePG, Schema: "MP_TEST", DSN: "dm://u:secret-password@localhost:5236", TimeoutSeconds: 1, StatementTimeoutSeconds: 1}
	var output bytes.Buffer
	start := time.Now()
	r := isolatedWorker(context.Background(), target, configuration{Targets: []Target{target}}, commandOptions{config: "unused.json"}, &output)
	if time.Since(start) > 5*time.Second {
		t.Fatal("worker deadline did not bound process")
	}
	if strings.Contains(output.String(), "secret-password") {
		t.Fatal("worker output exposed password")
	}
	statuses := map[string]Result{}
	for _, entry := range r.Results {
		statuses[entry.ID] = entry
	}
	if statuses["worker.deadline"].Status != Fail || !strings.Contains(statuses["worker.recovery"].Detail, "cleanup refused") {
		t.Fatalf("worker outcome: %+v", r.Results[:3])
	}
	if statuses["workflow.run"].Status != Blocked {
		t.Fatal("interrupted coverage disappeared")
	}
}

func TestRecoveryRequiresPersistedSuccessfulPreflight(t *testing.T) {
	for _, previous := range []TargetReport{{}, {Server: serverInfo{Fingerprint: testServer}}, {Server: serverInfo{Fingerprint: testServer}, Results: []Result{{ID: "preflight", Status: Fail}}}} {
		if err := recoverOwned(Target{}, previous); err == nil || !strings.Contains(err.Error(), "cleanup refused") {
			t.Fatalf("unproven cleanup=%v", err)
		}
	}
}

func TestCapturedWorkerOutputIsBounded(t *testing.T) {
	var b boundedOutput
	p := bytes.Repeat([]byte("x"), 2*1024*1024)
	if n, err := b.Write(p); err != nil || n != len(p) || b.Len() != 1024*1024 {
		t.Fatalf("bounded write=%d/%d/%v", n, b.Len(), err)
	}
}

func TestSamePathComparesExistingAndMissingPaths(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "targets.json")
	report := filepath.Join(dir, "report.json")
	for _, name := range []string{config, report} {
		if err := os.WriteFile(name, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if !samePath(config, config) || samePath(config, report) {
		t.Fatal("existing-file comparison broke")
	}
	// Missing paths compare lexically, case-insensitively only on Windows.
	missing := filepath.Join(dir, "missing-report.json")
	if samePath(config, missing) || samePath(report, missing) {
		t.Fatal("existing path matched a missing path")
	}
	if got := samePath(missing, filepath.Join(dir, "Missing-Report.JSON")); got != (runtime.GOOS == "windows") {
		t.Fatalf("missing-path case comparison=%v on %s", got, runtime.GOOS)
	}
}

func TestSamePathRecognizesSymlinkAliases(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "targets.json")
	report := filepath.Join(dir, "report.json")
	for _, name := range []string{config, report} {
		if err := os.WriteFile(name, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(dir, "alias.json")
	if err := os.Symlink(config, alias); err != nil {
		t.Skipf("file symlinks unavailable: %v", err)
	}
	if !samePath(config, alias) || !samePath(alias, config) {
		t.Fatal("symlink alias not recognized")
	}
	linkedDir := filepath.Join(dir, "linked")
	if err := os.Symlink(dir, linkedDir); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	if !samePath(report, filepath.Join(linkedDir, "report.json")) {
		t.Fatal("symlinked ancestor not resolved for existing path")
	}
	if samePath(config, filepath.Join(linkedDir, "new-report.json")) {
		t.Fatal("distinct missing path matched")
	}
	if !samePath(filepath.Join(dir, "new-report.json"), filepath.Join(linkedDir, "new-report.json")) {
		t.Fatal("missing report behind symlinked ancestor not resolved")
	}
}
