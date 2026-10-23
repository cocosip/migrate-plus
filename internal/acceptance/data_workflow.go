package acceptance

import (
	"fmt"
	"testing/fstest"
)

var constraintClasses = []string{errorUnique, errorNull, errorCheck, errorForeignKey}

func (s *suite) dataWorkflows() {
	for _, class := range constraintClasses {
		s.isolated("workflow.constraint-"+class, func() error { return s.constraintMigration(class) })
	}
	s.isolated("workflow.data-widening", s.dataWidening)
}

func (s *suite) constraintMigration(class string) error {
	declarations := map[string]string{
		errorUnique:     "v INTEGER UNIQUE",
		errorNull:       "v INTEGER NOT NULL",
		errorCheck:      "v INTEGER CHECK(v>0)",
		errorForeignKey: "v INTEGER, CONSTRAINT mp_type_fk FOREIGN KEY(v) REFERENCES mp_probe(id)",
	}
	values := map[string]string{errorUnique: "7", errorNull: sqlNull, errorCheck: "-1", errorForeignKey: "8"}
	files := fstest.MapFS{
		createUpFile:         {Data: []byte("CREATE TABLE mp_probe(id INTEGER PRIMARY KEY); INSERT INTO mp_probe VALUES(7); CREATE TABLE mp_type(id INTEGER PRIMARY KEY," + declarations[class] + "); INSERT INTO mp_type VALUES(1,7);")},
		createDownFile:       {Data: []byte(s.dropTable("mp_type") + ";" + s.dropTable("mp_probe") + ";")},
		"2_violate.up.sql":   {Data: []byte("INSERT INTO mp_type VALUES(2," + values[class] + ");")},
		"2_violate.down.sql": {Data: []byte("DELETE FROM mp_type WHERE id=2;")},
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
	if err := s.want("SELECT COUNT(*) FROM mp_type", "1"); err != nil {
		return err
	}
	if err := s.m.Force(1); err != nil {
		return err
	}
	if err := s.m.Down(); err != nil {
		return err
	}
	return s.version(-1, false)
}

func (s *suite) wideningSQL() (initial, alter, integerRead, numericRead string) {
	integer, text := typeSmallInt, "VARCHAR(20)"
	integerRead, numericRead = "CAST(v AS VARCHAR(100))", "CAST(amount AS VARCHAR(100))"
	switch s.profile.Family {
	case driverOracle:
		integer, text = "NUMBER(5)", "VARCHAR2(20 CHAR)"
		alter = "ALTER TABLE mp_type MODIFY (v NUMBER(19), label VARCHAR2(200 CHAR), amount DECIMAL(20,4));"
		integerRead, numericRead = "TO_CHAR(v)", "TO_CHAR(amount,'FM9999999999999990D0000','NLS_NUMERIC_CHARACTERS=''.,''')"
	case driverDM:
		alter = "ALTER TABLE mp_type MODIFY v BIGINT; ALTER TABLE mp_type MODIFY label VARCHAR(200); ALTER TABLE mp_type MODIFY amount DECIMAL(20,4);"
	case modeMySQL:
		alter = "ALTER TABLE mp_type MODIFY COLUMN v BIGINT, MODIFY COLUMN label VARCHAR(200), MODIFY COLUMN amount DECIMAL(20,4);"
		integerRead, numericRead = "CAST(v AS CHAR)", "CAST(amount AS CHAR)"
	default:
		alter = "ALTER TABLE mp_type ALTER COLUMN v TYPE BIGINT; ALTER TABLE mp_type ALTER COLUMN label TYPE VARCHAR(200); ALTER TABLE mp_type ALTER COLUMN amount TYPE DECIMAL(20,4);"
	}
	initial = "(id INTEGER PRIMARY KEY,v " + integer + ", label " + text + ", amount DECIMAL(10,2))"
	return initial, alter, integerRead, numericRead
}

func (s *suite) dataWidening() error {
	initial, alter, integerRead, numericRead := s.wideningSQL()
	// Rebuild on down: Oracle forbids reducing populated NUMBER precision even for fitting rows.
	down := "UPDATE mp_type SET v=32767,label='initial',amount=12345.67; CREATE TABLE mp_probe" + initial + "; INSERT INTO mp_probe SELECT * FROM mp_type; " + s.dropTable("mp_type") + "; ALTER TABLE mp_probe RENAME TO mp_type;"
	files := fstest.MapFS{
		createUpFile:       {Data: []byte("CREATE TABLE mp_type" + initial + "; INSERT INTO mp_type VALUES(1,32767,'initial',12345.67);")},
		createDownFile:     {Data: []byte(s.dropTable("mp_type") + ";")},
		"2_widen.up.sql":   {Data: []byte(alter + " UPDATE mp_type SET v=9007199254740993,label='Unicode 中文 after widening',amount=12345678901234.5678;")},
		"2_widen.down.sql": {Data: []byte(down)},
	}
	if err := s.open(files, "instance"); err != nil {
		return err
	}
	if err := s.m.Up(); err != nil {
		return err
	}
	if err := s.assertWidened(integerRead, numericRead); err != nil {
		return err
	}
	if err := s.m.Migrate(1); err != nil {
		return err
	}
	if err := s.want("SELECT "+integerRead+" FROM mp_type", "32767"); err != nil {
		return err
	}
	if err := s.want("SELECT label FROM mp_type", "initial"); err != nil {
		return err
	}
	if err := s.m.Up(); err != nil {
		return err
	}
	return s.assertWidened(integerRead, numericRead)
}

func (s *suite) assertWidened(integerRead, numericRead string) error {
	for query, want := range map[string]string{integerRead: "9007199254740993", numericRead: "12345678901234.5678", "label": "Unicode 中文 after widening"} {
		if err := s.want("SELECT "+query+" FROM mp_type", want); err != nil {
			return err
		}
	}
	ctx, cancel := s.operationContext()
	defer cancel()
	column, err := s.c.column(ctx, "mp_type", "label")
	if err != nil {
		return err
	}
	if column.Length != 200 {
		return fmt.Errorf("widened label metadata length=%d, want 200", column.Length)
	}
	column, err = s.c.column(ctx, "mp_type", "amount")
	if err != nil {
		return err
	}
	if err = verifyTypeMetadata(typeCase{Name: "widened amount", DDL: "DECIMAL(20,4)"}, column); err != nil {
		return err
	}
	return s.version(2, false)
}
