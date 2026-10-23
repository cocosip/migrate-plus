package acceptance

import "fmt"

type objectFixture struct{ ID, Setup, Query, Expected, Reason string }

func objectFixtures(p profile) []objectFixture {
	result := []objectFixture{}
	add := func(id, setup, query, want string) {
		result = append(result, objectFixture{ID: id, Setup: setup, Query: query, Expected: want})
	}
	switch p.Family {
	case driverOracle, driverDM:
		add("trigger", "CREATE OR REPLACE TRIGGER mp_trigger BEFORE INSERT ON mp_probe FOR EACH ROW BEGIN :NEW.id := :NEW.id+1; END;\n/\nINSERT INTO mp_probe VALUES(7);", "SELECT id FROM mp_probe", "8")
		add("synonym", "CREATE SYNONYM mp_synonym FOR mp_probe; INSERT INTO mp_synonym VALUES(7);", "SELECT id FROM mp_probe", "7")
		add("package", "CREATE OR REPLACE PACKAGE mp_pkg AS PROCEDURE put_row; END;\n/\nCREATE OR REPLACE PACKAGE BODY mp_pkg AS PROCEDURE put_row AS BEGIN INSERT INTO mp_probe VALUES(7); END; END;\n/\nBEGIN mp_pkg.put_row; END;\n/", "SELECT id FROM mp_probe", "7")
		if p.Driver != driverOceanBase {
			add("materialized-view", "INSERT INTO mp_probe VALUES(7); CREATE MATERIALIZED VIEW mp_mv AS SELECT id FROM mp_probe;", "SELECT id FROM mp_mv", "7")
		}
		if p.Family == driverDM {
			result = append(result, objectFixture{ID: "class", Reason: "DM CLASS creation/drop requires the exact native server catalog/DDL contract; not inferred from TYPE support"})
		}
	case modePG:
		add("materialized-view", "INSERT INTO mp_probe VALUES(7); CREATE MATERIALIZED VIEW mp_mv AS SELECT id FROM mp_probe;", "SELECT id FROM mp_mv", "7")
		add("trigger", "CREATE FUNCTION mp_fn() RETURNS trigger AS $$ BEGIN NEW.id:=NEW.id+1; RETURN NEW; END $$ LANGUAGE plpgsql; CREATE TRIGGER mp_trigger BEFORE INSERT ON mp_probe FOR EACH ROW EXECUTE PROCEDURE mp_fn(); INSERT INTO mp_probe VALUES(7);", "SELECT id FROM mp_probe", "8")
	case modeMySQL:
		add("trigger", "CREATE TRIGGER mp_trigger BEFORE INSERT ON mp_probe FOR EACH ROW SET NEW.id=NEW.id+1; INSERT INTO mp_probe VALUES(7);", "SELECT id FROM mp_probe", "8")
	}
	return result
}

func (s *suite) objectCases() {
	for _, fixture := range objectFixtures(s.profile) {
		if fixture.Reason != "" {
			s.record("object."+fixture.ID, Skipped, fmt.Errorf("%s", fixture.Reason), 0)
			continue
		}
		s.isolated("object."+fixture.ID, func() error {
			if err := s.exec("CREATE TABLE mp_probe(id INTEGER PRIMARY KEY)"); err != nil {
				return err
			}
			if err := s.exec(fixture.Setup); err != nil {
				return err
			}
			if err := s.want(fixture.Query, fixture.Expected); err != nil {
				return err
			}
			ctx, cancel := s.operationContext()
			objects, err := s.c.inventory(ctx)
			cancel()
			if err != nil {
				return err
			}
			if err := checkOwned(objects); err != nil {
				return err
			}
			if err := s.d.Drop(); err != nil {
				return err
			}
			if err := s.closeMigrator(); err != nil {
				return err
			}
			return s.requireEmpty()
		})
	}
}
