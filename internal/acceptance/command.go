package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type commandOptions struct {
	config, target, report string
	list, worker           bool
}

// Main runs the repository regression command and returns its process exit code.
func Main(ctx context.Context, args []string, out io.Writer, version string) int {
	opts, err := parseOptions(args, out)
	if err != nil {
		return 2
	}
	if err = VerifyManifest(); err != nil {
		_, _ = fmt.Fprintln(out, err)
		return 2
	}
	cfg, err := loadConfiguration(opts)
	if err != nil {
		_, _ = fmt.Fprintln(out, err)
		return 2
	}
	targets, err := cfg.selectTargets(opts.target)
	if err != nil {
		_, _ = fmt.Fprintln(out, err)
		return 2
	}
	if opts.list {
		cfg.Targets = targets
		return listCases(cfg, out)
	}
	if opts.worker && len(targets) != 1 {
		_, _ = fmt.Fprintln(out, "worker requires one selected target")
		return 2
	}
	return executeTargets(ctx, opts, cfg, targets, out, version)
}

func parseOptions(args []string, out io.Writer) (commandOptions, error) {
	var opts commandOptions
	flags := flag.NewFlagSet("migrate-test", flag.ContinueOnError)
	flags.SetOutput(out)
	flags.StringVar(&opts.config, "config", "", "JSON target configuration")
	flags.StringVar(&opts.target, "target", "", "selected target name (default: all configured)")
	flags.StringVar(&opts.report, "report", "migration-report.json", "incremental JSON report")
	flags.BoolVar(&opts.list, "list", false, "list coverage without connecting")
	flags.BoolVar(&opts.worker, "worker", false, "internal isolated target worker")
	if err := flags.Parse(args); err != nil {
		return opts, err
	}
	if flags.NArg() != 0 {
		return opts, fmt.Errorf("unexpected positional arguments")
	}
	if opts.report == "" {
		return opts, fmt.Errorf("report path is required")
	}
	if opts.config != "" {
		configPath, err := filepath.Abs(opts.config)
		if err != nil {
			return opts, err
		}
		reportPath, err := filepath.Abs(opts.report)
		if err != nil {
			return opts, err
		}
		if samePath(configPath, reportPath) {
			return opts, fmt.Errorf("report must not overwrite configuration")
		}
	}
	return opts, nil
}

func loadConfiguration(opts commandOptions) (configuration, error) {
	if opts.list && opts.config == "" && opts.target == "" {
		return configuration{}, nil
	}
	if opts.config == "" {
		return configuration{}, fmt.Errorf("-config is required")
	}
	f, err := os.Open(opts.config)
	if err != nil {
		return configuration{}, fmt.Errorf("cannot open target configuration")
	}
	defer func() { _ = f.Close() }()
	return readConfig(f)
}

func listCases(cfg configuration, out io.Writer) int {
	entries := List("", "")
	if len(cfg.Targets) != 0 {
		entries = nil
		seen := map[string]bool{}
		for _, target := range cfg.Targets {
			for _, entry := range List(target.Driver, target.Mode) {
				// Several targets can share one profile; each profile's manifest is identical.
				key := entry.Profile + "\x00" + entry.ID
				if seen[key] {
					continue
				}
				seen[key] = true
				entries = append(entries, entry)
			}
		}
	}
	if err := json.NewEncoder(out).Encode(entries); err != nil {
		return 2
	}
	return 0
}

func executeTargets(ctx context.Context, opts commandOptions, cfg configuration, targets []Target, out io.Writer, version string) int {
	report := Report{Version: version, Started: time.Now().UTC()}
	selected := map[string]bool{}
	for _, target := range targets {
		selected[target.Name] = true
	}
	for _, target := range cfg.Targets {
		entry := Selection{Name: target.Name, Profile: target.Driver + "-" + target.Mode}
		report.Configured = append(report.Configured, entry)
		if !selected[target.Name] {
			report.Unselected = append(report.Unselected, entry)
		}
	}
	if err := report.Save(opts.report); err != nil {
		_, _ = fmt.Fprintln(out, "cannot initialize report:", err)
		return 1
	}
	for _, target := range targets {
		var result TargetReport
		if opts.worker {
			result = runWorker(ctx, target, cfg.Targets, &report, opts.report, out)
		} else if _, err := target.resolveDSN(); err != nil {
			result = Run(ctx, target, cfg.Targets, nil)
		} else {
			result = isolatedWorker(ctx, target, cfg, opts, out)
		}
		fillMissing(target, &result)
		report.Targets = append(report.Targets, result)
		if err := report.Save(opts.report); err != nil {
			_, _ = fmt.Fprintln(out, "cannot save report:", err)
			return 1
		}
	}
	_, _ = fmt.Fprintf(out, "counts=%v complete=%v\n", report.Counts(), report.ExitCode() == 0)
	return report.ExitCode()
}

func runWorker(ctx context.Context, target Target, all []Target, report *Report, path string, out io.Writer) TargetReport {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(target.TimeoutSeconds)*time.Second)
	defer cancel()
	var writeErr error
	result := Run(ctx, target, all, func(partial TargetReport) {
		copyReport := *report
		copyReport.Targets = append(append([]TargetReport{}, report.Targets...), partial)
		if err := copyReport.Save(path); err != nil {
			writeErr = errors.Join(writeErr, err)
			cancel()
		}
		last := partial.Results[len(partial.Results)-1]
		_, _ = fmt.Fprintf(out, "%s %s %s %s\n", target.Name, last.ID, last.Status, last.Detail)
	})
	if writeErr != nil {
		result.Results = append(result.Results, Result{ID: "report.write", Status: Fail, Detail: "incremental report write failed"})
	}
	return result
}
