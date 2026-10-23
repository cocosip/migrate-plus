package acceptance

import (
	"bytes"
	"context"
	"fmt"
	"os"
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
