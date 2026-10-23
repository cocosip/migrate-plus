package acceptance

import "fmt"

const (
	caseConstructorURL          = "constructor.url"
	caseWorkflowDownReapply     = "workflow.down-reapply"
	caseWorkflowDirtyUp         = "workflow.dirty-up"
	caseDriverDropDependencies  = "driver.drop-dependencies"
	caseDriverClosedSession     = "driver.closed-session"
	caseDriverLockDdlContention = "driver.lock-ddl-contention"
	caseDriverScriptFraming     = "driver.script-framing"
	caseDriverVersionRoundtrip  = "driver.version-roundtrip"
	caseDriverHistoryCorrupt    = "driver.history-corrupt"
	caseDriverDropCycle         = "driver.drop-cycle"
)

const (
	caseWorkflowStepsUp        = "workflow.steps-up"
	caseDriverLockLifecycle    = "driver.lock-lifecycle"
	caseDriverScriptRejections = "driver.script-rejections"
	caseDriverTransactions     = "driver.transactions"
)

// Kept explicit so an upstream API change requires reviewing the coverage contract.
var apiCases = map[string][]string{
	"migrate.New":                     {caseConstructorURL},
	"migrate.NewWithSourceInstance":   {"constructor.source"},
	"migrate.NewWithDatabaseInstance": {"constructor.database"},
	"migrate.NewWithInstance":         {"constructor.instance"},
	"migrate.Up":                      {caseWorkflowStepsUp, caseWorkflowDownReapply},
	"migrate.Down":                    {caseWorkflowDownReapply, "workflow.dirty-down"},
	"migrate.Steps":                   {caseWorkflowStepsUp, "workflow.step-boundaries"},
	"migrate.Migrate":                 {caseWorkflowDownReapply, "workflow.missing-version"},
	"migrate.Run":                     {"workflow.run", "workflow.nil-migration", caseWorkflowDirtyUp},
	"migrate.Version":                 {caseWorkflowStepsUp, caseWorkflowDirtyUp},
	"migrate.Force":                   {"workflow.force", caseWorkflowDirtyUp},
	"migrate.Drop":                    {caseDriverDropDependencies},
	"migrate.Close":                   {"source.close-failure", "connection.close"},
	"driver.Open":                     {caseConstructorURL},
	"driver.Close":                    {caseDriverClosedSession, caseDriverLockLifecycle},
	"driver.Lock":                     {caseDriverLockLifecycle, caseDriverLockDdlContention, "workflow.lock-timeout"},
	"driver.Unlock":                   {caseDriverLockLifecycle},
	"driver.Run":                      {caseDriverScriptFraming, caseDriverScriptRejections, caseDriverTransactions},
	"driver.SetVersion":               {caseDriverVersionRoundtrip},
	"driver.Version":                  {caseDriverVersionRoundtrip, caseDriverHistoryCorrupt},
	"driver.Drop":                     {caseDriverDropCycle, "cross-schema"},
}

// CaseInfo exposes every selected profile/case before connecting to a database.
type CaseInfo struct {
	Profile     string `json:"profile"`
	ID          string `json:"id"`
	Requirement string `json:"requirement,omitempty"`
}

// List returns the executable manifest; empty driver/mode includes all profiles.
func List(driver, mode string) []CaseInfo {
	var result []CaseInfo
	for _, p := range profiles() {
		if driver != "" && p.Driver != driver || mode != "" && p.Mode != mode {
			continue
		}
		add := func(id, requirement string) { result = append(result, CaseInfo{p.ID, id, requirement}) }
		add("preflight", "")
		for _, id := range dataWorkflowNames() {
			add(id, "")
			add(id+".cleanup", "")
		}
		for _, id := range workflowNames {
			add(id, "")
			add(id+".cleanup", "")
		}
		for _, id := range driverNames {
			add(id, "")
			add(id+".cleanup", "")
		}
		for _, ddl := range (&suite{profile: p}).historyTypes() {
			id := "history." + typeID(ddl)
			add(id, "")
			add(id+".cleanup", "")
		}
		for _, tc := range p.Types {
			add("type."+tc.ID, tc.Reason)
			if tc.Reason == "" {
				add("type."+tc.ID+".cleanup", "")
			}
		}
		for _, fixture := range objectFixtures(p) {
			id := "object." + fixture.ID
			add(id, fixture.Reason)
			if fixture.Reason == "" {
				add(id+".cleanup", "")
			}
		}
		add("cross-schema", "other_target: distinct disposable schema in same database/tenant with explicit SELECT/REFERENCES privileges")
		add("fault.external-disconnect", "external fault-injection facility")
		add("connection.close", "")
	}
	return result
}

func dataWorkflowNames() []string {
	ids := []string{"workflow.data-widening"}
	for _, class := range constraintClasses {
		ids = append(ids, "workflow.constraint-"+class)
	}
	return ids
}

func (s *suite) blockRemaining() {
	for _, entry := range List(s.target.Driver, s.target.Mode) {
		if s.status(entry.ID) == Blocked && !s.hasResult(entry.ID) {
			s.record(entry.ID, Blocked, fmt.Errorf("target preflight did not execute successfully"), 0)
		}
	}
}

func (s *suite) hasResult(id string) bool {
	for _, r := range s.report.Results {
		if r.ID == id {
			return true
		}
	}
	return false
}

// VerifyManifest rejects duplicate IDs and missing driver/API/type family coverage.
func VerifyManifest() error {
	for _, p := range profiles() {
		seen := map[string]bool{}
		for _, entry := range List(p.Driver, p.Mode) {
			if entry.ID == "" || seen[entry.ID] {
				return fmt.Errorf("invalid/duplicate manifest ID in %s: %s", p.ID, entry.ID)
			}
			seen[entry.ID] = true
		}
		for _, id := range append(append([]string{}, workflowNames...), driverNames...) {
			if !seen[id] {
				return fmt.Errorf("missing %s/%s", p.ID, id)
			}
		}
		for _, tc := range p.Types {
			if !seen["type."+tc.ID] {
				return fmt.Errorf("missing native type %s/%s", p.ID, tc.ID)
			}
		}
		for operation, ids := range apiCases {
			if len(ids) == 0 {
				return fmt.Errorf("unmapped API %s", operation)
			}
			for _, id := range ids {
				if !seen[id] {
					return fmt.Errorf("missing %s/%s for %s", p.ID, id, operation)
				}
			}
		}

	}
	return nil
}
