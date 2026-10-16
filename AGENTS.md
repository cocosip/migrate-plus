# Repository instructions

## Required completion checks

After the final changes, run all of the following from the repository root
before reporting completion or committing:

```powershell
$env:GOCACHE = Join-Path (Get-Location).Path '.cache/go-build'
$env:GOMODCACHE = Join-Path (Get-Location).Path '.cache/go-mod'
$env:GOLANGCI_LINT_CACHE = Join-Path (Get-Location).Path '.cache/golangci-lint'

go build ./...
go test ./... -count=1
golangci-lint run ./...
git diff --check
```

Every check must exit with code 0. Fix failures and rerun the checks before
claiming the change is complete. Do not substitute package-only checks, cached
test results, or disabled lint rules for these full repository checks.
Report the actual results and any remaining failures or skipped verification.

Database integration tests are separate from unit tests and require an
explicitly selected disposable schema. Never run destructive integration tests
against production GOIMAGE. A passing unit suite does not establish real-server
compatibility; report integration results and untested modes separately.
