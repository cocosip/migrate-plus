package acceptance

import (
	"io"
	"io/fs"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4/source/iofs"
)

func TestProfileFixturesAndTypeAssertions(t *testing.T) {
	all := profiles()
	if len(all) != 11 {
		t.Fatalf("profiles = %d, want 11", len(all))
	}
	seen := map[string]bool{}
	for _, p := range all {
		t.Run(p.ID, func(t *testing.T) {
			if seen[p.ID] {
				t.Fatalf("duplicate profile %s", p.ID)
			}
			seen[p.ID] = true
			selected, err := profileFor(p.Driver, p.Mode)
			if err != nil || selected.ID != p.ID {
				t.Fatalf("profile selection: %v, %s", err, selected.ID)
			}
			if p.Driver == "dm" && p.Family != "dm" {
				t.Fatal("DM modes must use DM-native SQL")
			}
			source, err := iofs.New(p.FS, ".")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := source.Close(); err != nil {
					t.Error(err)
				}
			})
			version, err := source.First()
			if err != nil || version != 0 {
				t.Fatalf("first migration = %d: %v", version, err)
			}
			for i, v := range []uint{0, 1, 2, 4, 202610230001} {
				for _, read := range []func(uint) (io.ReadCloser, string, error){source.ReadUp, source.ReadDown} {
					body, _, e := read(v)
					if e != nil {
						t.Fatal(e)
					}
					data, e := io.ReadAll(body)
					closeErr := body.Close()
					if e != nil || closeErr != nil || len(strings.TrimSpace(string(data))) == 0 {
						t.Fatalf("empty/unreadable migration %d", v)
					}
				}
				if i > 0 {
					next, e := source.Next(version)
					if e != nil || next != v {
						t.Fatalf("next(%d) = %d: %v", version, next, e)
					}
					version = next
				}
			}
			entries, err := fs.ReadDir(p.FS, ".")
			if err != nil || len(entries) != 10 {
				t.Fatalf("fixture files = %d: %v", len(entries), err)
			}
			checkTypeCases(t, p)
		})
	}
	if _, err := profileFor("dm", "a"); err == nil {
		t.Fatal("unsupported profile accepted")
	}
}

func TestExactTypeBoundaries(t *testing.T) {
	for _, p := range profiles() {
		byID := map[string]typeCase{}
		for _, c := range p.Types {
			byID[c.ID] = c
		}
		if c := byID["integer.above-2p53"]; c.Expected != "9007199254740993" || c.Updated != "-9007199254740993" {
			t.Errorf("%s loses integer precision: %+v", p.ID, c)
		}
		if c := byID["numeric.precision-38"]; c.Expected != "1234567890123456789012345678901234.5678" || c.Updated != "-1234567890123456789012345678901234.5678" {
			t.Errorf("%s loses decimal precision: %+v", p.ID, c)
		}
		if c := byID["text.unicode"]; c.Expected != "中文𐐷'\n;" || c.Updated != "更新𐐷" {
			t.Errorf("%s loses Unicode/literal framing: %+v", p.ID, c)
		}
		if c := byID["binary.zero-ff"]; c.Expected != "00FF0100" || c.Updated != "FF0001FF" {
			t.Errorf("%s loses binary bytes: %+v", p.ID, c)
		}
		if c := byID["lob.large-unicode"]; c.Reason != "" || len(c.Insert) < 100000 || c.Expected != "exact-initial" || c.Updated != "exact-updated" {
			t.Errorf("%s lacks executable large exact LOB assertion", p.ID)
		}
		if c := byID["binary.all-bytes"]; c.Reason != "" || len(c.Expected) != 512 || !strings.HasPrefix(c.Expected, "00010203") || !strings.HasSuffix(c.Expected, "FCFDFEFF") {
			t.Errorf("%s lacks all-byte boundary case", p.ID)
		}
		if c := byID["integer.above-2p53.null"]; c.Expected != "<NULL>" || c.Updated != "9007199254740993" {
			t.Errorf("%s lacks native null observation", p.ID)
		}
	}
}

func TestDMCompatibilityModesSelectDistinctNativeSQL(t *testing.T) {
	oracle, err := profileFor(driverDM, driverOracle)
	if err != nil {
		t.Fatal(err)
	}
	pg, err := profileFor(driverDM, modePG)
	if err != nil {
		t.Fatal(err)
	}
	oracleSQL, err := fs.ReadFile(oracle.FS, "0_initial.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	pgSQL, err := fs.ReadFile(pg.FS, "0_initial.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if string(oracleSQL) == string(pgSQL) || !strings.Contains(string(oracleSQL), "VARCHAR2(") || strings.Contains(string(pgSQL), "VARCHAR2(") {
		t.Fatal("DM Oracle and PG compatibility fixtures lost their native dialect distinction")
	}
	if emptyText(oracle) == emptyText(pg) {
		t.Fatal("DM Oracle empty-string semantics were merged with PG mode")
	}
}

func checkTypeCases(t *testing.T, p profile) {
	t.Helper()
	types := map[string]bool{}
	families := map[string]bool{}
	for _, c := range p.Types {
		if c.ID == "" || c.Name == "" || types[c.ID] {
			t.Fatalf("invalid/duplicate native type: %+v", c)
		}
		types[c.ID] = true
		family, _, ok := strings.Cut(c.ID, ".")
		if !ok {
			t.Fatalf("unclassified type %s", c.ID)
		}
		families[family] = true
		if c.Reason != "" {
			continue
		}
		if c.DDL == "" || c.Insert == "" || c.Update == "" || c.Query == "" || c.Expected == "" || c.Updated == "" {
			t.Fatalf("type missing executable assertion: %+v", c)
		}
		if !strings.HasPrefix(c.Query, "SELECT ") || !strings.Contains(c.Query, "mp_type") {
			t.Fatalf("not an independent observer query: %s", c.Query)
		}
	}
	for _, family := range []string{"integer", "numeric", "float", "boolean", "text", "lob", "binary", "temporal", "timezone", "interval", "structured"} {
		if !families[family] {
			t.Errorf("missing type family %s", family)
		}
	}
}
