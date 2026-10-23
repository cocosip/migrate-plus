package acceptance

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing/fstest"
)

func (s *suite) typeCases() {
	for _, tc := range s.profile.Types {
		id := "type." + tc.ID
		if tc.Reason != "" {
			status := Skipped
			if tc.ID == "interval.native" && s.profile.Family == modeMySQL {
				status = NotApplicable
			}
			s.record(id, status, fmt.Errorf("%s: %s", tc.Name, tc.Reason), 0)
			continue
		}
		s.isolated(id, func() error { return s.typeMigration(tc) })
	}
}

func (s *suite) typeMigration(tc typeCase) error {
	setup := tc.Setup
	if setup != "" {
		setup += "\n"
	}
	drop := s.dropTable("mp_type") + ";\n" + tc.Teardown
	files := fstest.MapFS{
		createUpFile:          {Data: []byte(setup + "CREATE TABLE mp_type(id INTEGER PRIMARY KEY, v " + tc.DDL + ");\nINSERT INTO mp_type(id,v) VALUES(1," + tc.Insert + ");")},
		createDownFile:        {Data: []byte(drop)},
		"2_update.up.sql":     {Data: []byte("UPDATE mp_type SET v=" + tc.Update + " WHERE id=1;")},
		"2_update.down.sql":   {Data: []byte("UPDATE mp_type SET v=" + tc.Insert + " WHERE id=1;")},
		"3_backfill.up.sql":   {Data: []byte("ALTER TABLE mp_type ADD marker INTEGER DEFAULT 7 NOT NULL;")},
		"3_backfill.down.sql": {Data: []byte("ALTER TABLE mp_type DROP COLUMN marker;")},
	}
	if err := s.open(files, "instance"); err != nil {
		return err
	}
	if err := s.m.Steps(1); err != nil {
		return fmt.Errorf("%s create/insert: %w", tc.Name, err)
	}
	if err := s.want(tc.Query, tc.Expected); err != nil {
		return err
	}
	if err := s.version(1, false); err != nil {
		return err
	}
	ctx, cancel := s.operationContext()
	column, err := s.c.column(ctx, "mp_type", "v")
	cancel()
	if err != nil {
		return err
	}
	if err := verifyTypeMetadata(tc, column); err != nil {
		return err
	}
	if err := s.m.Up(); err != nil {
		return fmt.Errorf("%s update/backfill: %w", tc.Name, err)
	}
	if err := s.want(tc.Query, tc.Updated); err != nil {
		return err
	}
	if err := s.want("SELECT marker FROM mp_type WHERE id=1", "7"); err != nil {
		return err
	}
	ctx, cancel = s.operationContext()
	marker, err := s.c.column(ctx, "mp_type", "marker")
	cancel()
	if err != nil {
		return err
	}
	if marker.Nullable || !strings.Contains(marker.Default, "7") {
		return fmt.Errorf("backfilled marker metadata=%+v", marker)
	}
	if err := s.m.Migrate(1); err != nil {
		return err
	}
	if err := s.want(tc.Query, tc.Expected); err != nil {
		return err
	}
	if err := s.m.Down(); err != nil {
		return err
	}
	if err := s.version(-1, false); err != nil {
		return err
	}
	if err := s.m.Up(); err != nil {
		return err
	}
	return s.want(tc.Query, tc.Updated)
}

var numericDeclaration = regexp.MustCompile(`(?i)^(?:NUMBER|NUMERIC|DECIMAL)\((\d+)(?:,(\d+))?\)$`)
var textDeclaration = regexp.MustCompile(`(?i)^(?:VAR)?(?:N)?CHAR(?:2)?\((\d+)(?: CHAR)?\)$`)

func verifyTypeMetadata(tc typeCase, column columnInfo) error {
	if strings.TrimSpace(column.Type) == "" {
		return fmt.Errorf("%s returned empty native type metadata", tc.Name)
	}
	declarations := tc.MetadataTypes
	if len(declarations) == 0 {
		declarations = []string{tc.DDL}
	}
	matched := false
	for _, declaration := range declarations {
		matched = matched || nativeTypeMatches(declaration, column.Type)
	}
	if !matched {
		return fmt.Errorf("%s native type=%q does not match declaration %q", tc.Name, column.Type, tc.DDL)
	}
	if !column.Nullable {
		return fmt.Errorf("%s unexpectedly non-nullable", tc.Name)
	}
	if m := numericDeclaration.FindStringSubmatch(tc.DDL); m != nil {
		precision, _ := strconv.ParseInt(m[1], 10, 64)
		scale := int64(0)
		if m[2] != "" {
			scale, _ = strconv.ParseInt(m[2], 10, 64)
		}
		if column.Precision != precision || column.Scale != scale {
			return fmt.Errorf("%s metadata precision/scale=%d/%d, want %d/%d", tc.Name, column.Precision, column.Scale, precision, scale)
		}
	}
	if m := textDeclaration.FindStringSubmatch(tc.DDL); m != nil {
		length, _ := strconv.ParseInt(m[1], 10, 64)
		if column.Length != length {
			return fmt.Errorf("%s metadata length=%d, want %d", tc.Name, column.Length, length)
		}
	}
	return nil
}

var typeParameters = regexp.MustCompile(`\([^)]*\)`)

func nativeTypeMatches(declaration, observed string) bool {
	canonical := func(value string) string {
		value = strings.Join(strings.Fields(strings.ToUpper(typeParameters.ReplaceAllString(value, ""))), " ")
		value = strings.TrimSuffix(value, " UNSIGNED")
		if strings.HasSuffix(value, "[]") {
			return "ARRAY"
		}
		aliases := map[string]string{
			"DECIMAL": typeNumber, "NUMERIC": typeNumber,
			"INT": typeInteger, "INT4": typeInteger, "INT2": typeSmallInt, "INT8": typeBigInt,
			"CHARACTER": "CHAR", "BPCHAR": "CHAR",
			"CHARACTER VARYING": "VARCHAR", typeVarchar2: "VARCHAR",
			"NVARCHAR": typeNVarchar2, "NATIONAL CHARACTER VARYING": typeNVarchar2,
			"DOUBLE PRECISION": typeDouble, "FLOAT8": typeDouble, "FLOAT4": typeReal,
			"BOOL": typeBoolean, "TIMESTAMP WITHOUT TIME ZONE": typeTimestamp, "TIME WITHOUT TIME ZONE": "TIME",
		}
		if alias, ok := aliases[value]; ok {
			return alias
		}
		return value
	}
	return canonical(declaration) == canonical(observed)
}

func (s *suite) generated() error {
	ddl := "INTEGER GENERATED BY DEFAULT AS IDENTITY"
	switch s.profile.Family {
	case modeMySQL:
		ddl = "INTEGER AUTO_INCREMENT"
	case driverDM:
		ddl = "INTEGER IDENTITY(1,1)"
	}
	if err := s.exec("CREATE TABLE mp_probe(id " + ddl + " PRIMARY KEY, label VARCHAR(20) DEFAULT 'generated' NOT NULL); INSERT INTO mp_probe(label) VALUES('first'); INSERT INTO mp_probe(label) VALUES('second');"); err != nil {
		return err
	}
	if err := s.want("SELECT COUNT(DISTINCT id) FROM mp_probe", "2"); err != nil {
		return err
	}
	if err := s.exec("UPDATE mp_probe SET label='backfill' WHERE label='first'; INSERT INTO mp_probe(label) VALUES('third');"); err != nil {
		return err
	}
	if err := s.want("SELECT COUNT(DISTINCT id) FROM mp_probe", "3"); err != nil {
		return err
	}
	return s.want("SELECT COUNT(*) FROM mp_probe WHERE label='backfill'", "1")
}

func (s *suite) constraints() error {
	if err := s.exec("CREATE TABLE mp_probe(id INTEGER PRIMARY KEY, v INTEGER DEFAULT 7 NOT NULL, CONSTRAINT mp_probe_unique UNIQUE(v), CONSTRAINT mp_probe_check CHECK(v>0)); INSERT INTO mp_probe(id) VALUES(1);"); err != nil {
		return err
	}
	if err := s.want("SELECT v FROM mp_probe WHERE id=1", "7"); err != nil {
		return err
	}
	for _, v := range []struct{ sql, class string }{{"INSERT INTO mp_probe VALUES(2,7)", errorUnique}, {"INSERT INTO mp_probe VALUES(3,NULL)", errorNull}, {"INSERT INTO mp_probe VALUES(4,-1)", errorCheck}} {
		if err := expectError(s.exec(v.sql), v.class); err != nil {
			return err
		}
	}
	return s.want("SELECT COUNT(*) FROM mp_probe", "1")
}

func (s *suite) negativeMigration(ddl, value, class string) error {
	files := fstest.MapFS{
		createUpFile:     {Data: []byte("CREATE TABLE mp_type(id INTEGER PRIMARY KEY,v " + ddl + "); INSERT INTO mp_type VALUES(1,1);")},
		createDownFile:   {Data: []byte(s.dropTable("mp_type") + ";")},
		"2_bad.up.sql":   {Data: []byte("INSERT INTO mp_type VALUES(2," + value + ");")},
		"2_bad.down.sql": {Data: []byte("DELETE FROM mp_type WHERE id=2;")},
	}
	if err := s.open(files, "instance"); err != nil {
		return err
	}
	if err := expectError(s.m.Up(), class); err != nil {
		return err
	}
	if err := s.version(2, true); err != nil {
		return err
	}
	return s.want("SELECT COUNT(*) FROM mp_type", "1")
}

func (s *suite) integerOverflow() error {
	if s.profile.Family == driverOracle {
		return s.negativeMigration("NUMBER(3)", "1000", "overflow")
	}
	return s.negativeMigration(typeSmallInt, "32768", "overflow")
}

func (s *suite) numericNarrowing() error {
	if err := s.exec("CREATE TABLE mp_type(id INTEGER PRIMARY KEY,v DECIMAL(10,2)); INSERT INTO mp_type VALUES(1,12345.67);"); err != nil {
		return err
	}
	alter := "ALTER TABLE mp_type ALTER COLUMN v TYPE DECIMAL(3,2)"
	switch s.profile.Family {
	case driverOracle, driverDM:
		alter = "ALTER TABLE mp_type MODIFY v DECIMAL(3,2)"
	case modeMySQL:
		alter = "ALTER TABLE mp_type MODIFY COLUMN v DECIMAL(3,2)"
	}
	if err := s.closeMigrator(); err != nil {
		return err
	}
	if err := s.open(fstest.MapFS{"1_narrow.up.sql": {Data: []byte(alter)}, "1_narrow.down.sql": {Data: []byte("-- explicit inspection required")}}, "instance"); err != nil {
		return err
	}
	err := s.m.Up()
	if err == nil {
		return fmt.Errorf("out-of-range narrowing succeeded")
	}
	text := strings.ToLower(err.Error())
	if !strings.Contains(text, "ora-01440") && !strings.Contains(text, "ora-01439") {
		if e := expectError(err, "overflow"); e != nil {
			return e
		}
	}
	if err := s.version(1, true); err != nil {
		return err
	}
	return s.want("SELECT COUNT(*) FROM mp_type WHERE v=12345.67", "1")
}

func (s *suite) literalBytes() error {
	// Backslash-sensitive literals are asserted as native bytes, with no fallback rewrite.
	escapes, err := s.backslashEscapes()
	if err != nil {
		return err
	}
	value := "line1\\\\line2\n'';end"
	if err := s.exec("CREATE TABLE mp_probe(v VARCHAR(200)); INSERT INTO mp_probe VALUES('" + value + "');"); err != nil {
		return err
	}
	expected := "line1\\\\line2\n';end"
	if escapes {
		expected = "line1\\line2\n';end"
	}
	return s.want("SELECT v FROM mp_probe", expected)
}

func (s *suite) backslashEscapes() (bool, error) {
	switch s.profile.Family {
	case modeMySQL:
		mode, err := s.scalar("SELECT @@SESSION.sql_mode")
		if err != nil {
			return false, err
		}
		for _, option := range strings.Split(mode, ",") {
			if strings.EqualFold(strings.TrimSpace(option), "NO_BACKSLASH_ESCAPES") {
				return false, nil
			}
		}
		return true, nil
	case modePG:
		setting, err := s.scalar("SELECT pg_catalog.current_setting('standard_conforming_strings')")
		if err != nil {
			return false, err
		}
		if setting != "on" && setting != "off" {
			return false, fmt.Errorf("unknown standard_conforming_strings %q", setting)
		}
		return setting == "off", nil
	case driverDM:
		setting, err := s.scalar("SELECT SF_GET_PARA_VALUE(2, 'BACKSLASH_ESCAPE') FROM DUAL")
		if err != nil {
			return false, err
		}
		if setting != "0" && setting != "1" {
			return false, fmt.Errorf("unknown BACKSLASH_ESCAPE %q", setting)
		}
		return setting == "1", nil
	default:
		return false, nil
	}
}
