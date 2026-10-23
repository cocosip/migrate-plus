package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Keep output bounded even when a native client repeatedly logs a failure.
type boundedOutput struct{ bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 1024*1024 - b.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}

func isolatedWorker(ctx context.Context, target Target, cfg configuration, opts commandOptions, out io.Writer) TargetReport {
	result := TargetReport{Name: target.Name, Profile: target.Driver + "-" + target.Mode}
	f, err := os.CreateTemp("", "migrate-worker-*.json")
	if err != nil {
		return workerFailure(result, "worker.start", err)
	}
	path := f.Name()
	_ = f.Close()
	defer func() { _ = os.Remove(path) }()
	executable, err := os.Executable()
	if err != nil {
		return workerFailure(result, "worker.start", err)
	}
	workerCtx, cancel := context.WithTimeout(ctx, time.Duration(target.TimeoutSeconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(workerCtx, executable, "-worker", "-config", opts.config, "-target", target.Name, "-report", path)
	hideWorker(cmd)
	var output boundedOutput
	cmd.Stdout, cmd.Stderr = &output, &output
	runErr := cmd.Run()
	message := output.String()
	for _, t := range cfg.Targets {
		if dsn, e := t.resolveDSN(); e == nil {
			message = redactConnection(message, dsn)
		}
	}
	_, _ = io.WriteString(out, message)
	data, readErr := os.ReadFile(path)
	var partial Report
	if readErr == nil {
		readErr = json.Unmarshal(data, &partial)
	}
	if readErr == nil && len(partial.Targets) == 1 && partial.Targets[0].Name == target.Name {
		result = partial.Targets[0]
	} else {
		result = workerFailure(result, "worker.report", fmt.Errorf("worker did not preserve a valid partial report"))
	}
	if workerCtx.Err() != nil {
		result = workerFailure(result, "worker.deadline", workerCtx.Err())
		result = workerFailure(result, "worker.recovery", recoverOwned(target, result))
	} else if runErr != nil && !expectedWorkerExit(runErr, result) {
		result = workerFailure(result, "worker.exit", fmt.Errorf("worker terminated unexpectedly; inspect disposable schemas before reuse"))
	}
	fillMissing(target, &result)
	return result
}

func expectedWorkerExit(err error, result TargetReport) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 1 && (Report{Targets: []TargetReport{result}}).ExitCode() == 1
}

func workerFailure(result TargetReport, id string, err error) TargetReport {
	status := statusFor(err)
	entry := Result{ID: id, Status: status}
	if err != nil {
		entry.Detail = err.Error()
	}
	result.Results = append(result.Results, entry)
	return result
}

func fillMissing(target Target, result *TargetReport) {
	seen := map[string]bool{}
	for _, r := range result.Results {
		seen[r.ID] = true
	}
	for _, c := range List(target.Driver, target.Mode) {
		if !seen[c.ID] {
			result.Results = append(result.Results, Result{ID: c.ID, Status: Blocked, Detail: "worker did not complete case"})
		}
	}
}

func recoverOwned(target Target, previous TargetReport) (result error) {
	if !hasPreflight(previous) {
		return fmt.Errorf("no persisted empty-schema preflight; automatic cleanup refused")
	}
	if target.OtherTarget != "" {
		return fmt.Errorf("paired-schema worker interrupted; inspect both schemas manually before reuse")
	}
	dsn, err := target.resolveDSN()
	if err != nil {
		return err
	}
	defer func() {
		if result != nil {
			result = fmt.Errorf("%s", redactConnection(result.Error(), dsn))
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), target.statementTimeout())
	defer cancel()
	db, err := nativePool(target, dsn, target.statementTimeout())
	if err != nil {
		return err
	}
	c := &connection{db: db, target: target, dsn: dsn}
	defer func() { result = errors.Join(result, c.close()) }()
	c.conn, err = db.Conn(ctx)
	if err != nil {
		return err
	}
	c.info, c.server, c.tenant, err = c.identity(ctx)
	if err != nil {
		return err
	}
	info := c.info
	info.Fingerprint = c.server + "/" + c.tenant
	if info != previous.Server {
		return fmt.Errorf("server identity changed; cleanup refused")
	}
	return (&suite{ctx: ctx, c: c, target: target}).cleanup()
}

func hasPreflight(report TargetReport) bool {
	if report.Server.Fingerprint == "" {
		return false
	}
	for _, r := range report.Results {
		if r.ID == "preflight" && r.Status == Pass {
			return true
		}
	}
	return false
}

func samePath(a, b string) bool { return strings.EqualFold(a, b) }
