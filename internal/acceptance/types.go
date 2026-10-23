package acceptance

import (
	"encoding/hex"
	"strings"
)

const (
	nativeTypeBit        = "BIT"
	nativeTypeBlob       = "BLOB"
	nativeTypeText       = "TEXT"
	nativeTypeGeometry   = "GEOMETRY"
	nativeTypeStructured = "structured"
	dropNativeType       = "DROP TYPE mp_native_type;"
)

// Values are SQL literals and observer expressions, never float64 conversions.
func nativeTypes(p profile) []typeCase {
	var cases []typeCase
	add := func(id, name, ddl, insert, update, expression, expected, updated string) {
		cases = append(cases, typeCase{ID: id, Name: name, DDL: ddl, Insert: insert, Update: update, Query: "SELECT " + expression + " FROM mp_type WHERE id=1", Expected: expected, Updated: updated})
	}
	conditional := func(id, name, reason string) { cases = append(cases, typeCase{ID: id, Name: name, Reason: reason}) }
	cast := numericText(p)
	integer := typeBigInt
	if p.Family == driverOracle {
		integer = typeNumber19
	}
	add("integer.above-2p53", integer, integer, "9007199254740993", "-9007199254740993", cast, "9007199254740993", "-9007199254740993")
	add("integer.zero-negative", integer, integer, "0", "-1", cast, "0", "-1")
	if p.Family == driverOracle {
		add("integer.number-19", typeNumber19, typeNumber19, "-9223372036854775808", "9223372036854775807", cast, "-9223372036854775808", "9223372036854775807")
	} else {
		for _, width := range []struct{ id, ddl, min, max string }{{"smallint", typeSmallInt, "-32768", "32767"}, {"int", typeInteger, "-2147483648", "2147483647"}, {"bigint", typeBigInt, "-9223372036854775808", "9223372036854775807"}} {
			add("integer."+width.id, width.ddl, width.ddl, width.min, width.max, cast, width.min, width.max)
		}
	}
	decimal := "DECIMAL(38,4)"
	decimalRead := cast
	if p.Family == driverOracle {
		decimal = "NUMBER(38,4)"
		decimalRead = "TO_CHAR(v,'FM" + strings.Repeat("9", 33) + "0D0000','NLS_NUMERIC_CHARACTERS = ''.,''')"
	}
	if p.Family == driverDM {
		decimalRead = "TO_CHAR(v,'FM" + strings.Repeat("9", 33) + "0.0000')"
	}
	add("numeric.precision-38", decimal, decimal, "1234567890123456789012345678901234.5678", "-1234567890123456789012345678901234.5678", decimalRead, "1234567890123456789012345678901234.5678", "-1234567890123456789012345678901234.5678")
	add("numeric.scale-zero", decimal, decimal, "0.0000", "-1.2300", decimalRead, "0.0000", "-1.2300")
	floatNames := []string{typeReal, "DOUBLE PRECISION"}
	if p.Family == driverOracle {
		floatNames = []string{typeFloat, "BINARY_FLOAT", "BINARY_DOUBLE"}
	}
	for _, name := range floatNames {
		add("float."+typeID(name), name, name, "-1.5", "0.25", "CASE WHEN v=-1.5 THEN '-1.5' WHEN v=0.25 THEN '0.25' ELSE 'MISMATCH' END", "-1.5", "0.25")
	}
	boolean := typeBoolean
	if p.Family == driverOracle {
		conditional("boolean.native", typeBoolean, "Requires a declared server version supporting SQL BOOLEAN; Oracle 23c+ availability is unresolved for this target")
	} else {
		if p.Family == driverDM || p.Family == modeMySQL {
			boolean = nativeTypeBit
		}
		insert, update, expression := "FALSE", "TRUE", "CASE WHEN v THEN 'true' ELSE 'false' END"
		if boolean == nativeTypeBit {
			insert, update, expression = "0", "1", "CASE WHEN v=1 THEN 'true' ELSE 'false' END"
		}
		add("boolean.native", boolean, boolean, insert, update, expression, "false", "true")
	}
	varchar := "VARCHAR(200)"
	if p.Family == driverOracle {
		varchar = "VARCHAR2(200 CHAR)"
	}
	if p.Driver == driverDM && p.Mode == driverOracle {
		varchar = "VARCHAR2(200)"
	}
	add("text.unicode", varchar, varchar, "'中文𐐷''\n;'", "'更新𐐷'", "v", "中文𐐷'\n;", "更新𐐷")
	add("text.null", varchar, varchar, sqlNull, "'value'", "COALESCE(v,'<NULL>')", nullValue, "value")
	emptyRead := "CASE WHEN v IS NULL THEN '<NULL>' WHEN LENGTH(v)=0 THEN '<EMPTY>' ELSE v END"
	if p.Family == driverDM {
		// DM LENGTH('') is NULL even when native mode preserves the empty string.
		emptyRead = "CASE WHEN v IS NULL THEN '<NULL>' WHEN v='' THEN '<EMPTY>' ELSE v END"
	}
	add("text.empty", varchar, varchar, "''", "'x'", emptyRead, emptyText(p), "x")
	add("text.length-boundary", "VARCHAR(4)", "VARCHAR(4)", "'abcd'", "'wxyz'", "v", "abcd", "wxyz")
	charLength := "LENGTH(v)=4"
	if p.Family == modePG {
		charLength = "OCTET_LENGTH(v)=4"
	}
	if p.Family == modeMySQL {
		charLength = "LENGTH(v)=1"
	}
	add("text.char-padding", "CHAR(4)", "CHAR(4)", "'x'", "'abcd'", "CASE WHEN "+charLength+" AND v=CAST('x' AS CHAR(4)) THEN 'x-padded' WHEN LENGTH(v)=4 AND v='abcd' THEN 'abcd' ELSE 'MISMATCH' END", "x-padded", "abcd")
	switch p.Family {
	case modePG:
		pgTypes(&cases, add, conditional)
	case modeMySQL:
		mysqlTypes(&cases, add, conditional)
	case driverOracle:
		oracleTypes(&cases, add, conditional, p)
	default:
		dmTypes(add, conditional, p)
	}
	conditional("float.special-values", "NaN/Infinity", "Native availability and canonical readback must be declared for the exact server version; finite cases do not imply special-value support")
	if p.Mode == modeA {
		for _, name := range []string{typeNumber, typeVarchar2, typeNVarchar2, "RAW", typeClob, nativeTypeBlob, "BINARY_INTEGER", "SIMPLE_INTEGER"} {
			conditional("structured.a-alias-"+typeID(name), name, "A-mode Oracle alias availability and exact semantics require this product/version's native catalog contract")
		}
	}
	addLargeAndBinary(p, add)
	backslash := "CHR(92)||'a'"
	updateBackslash := "CHR(92)||'b'"
	if p.Family == modeMySQL {
		backslash = "CONCAT(CHAR(92),'a')"
		updateBackslash = "CONCAT(CHAR(92),'b')"
	}
	add("text.backslash", varchar, varchar, backslash, updateBackslash, "v", "\\a", "\\b")
	// Observe NULL independently for each scalar, without treating it as an empty value.
	count := len(cases)
	for _, c := range cases[:count] {
		if c.Reason != "" || c.Insert == sqlNull || c.ID == "lob.large-unicode" {
			continue
		}
		c.ID += ".null"
		c.Update, c.Updated = c.Insert, c.Expected
		c.Insert, c.Expected = sqlNull, nullValue
		marker := "'<NULL>'"
		if p.Family == driverOracle {
			switch name, _, _ := strings.Cut(c.DDL, "("); name {
			case "NCHAR", typeNVarchar2, "NCLOB":
				marker = "N'<NULL>'"
			}
		}
		c.Query = "SELECT CASE WHEN v IS NULL THEN " + marker + " ELSE (" + c.Query + ") END FROM mp_type WHERE id=1"
		cases = append(cases, c)
	}
	declareMetadataAliases(p, cases)
	return cases
}

func declareMetadataAliases(p profile, cases []typeCase) {
	for i := range cases {
		c := &cases[i]
		switch {
		case p.Family == modeMySQL && c.DDL == typeBoolean:
			c.MetadataTypes = []string{"TINYINT"}
		case p.Family == driverDM && c.DDL == typeBoolean:
			c.MetadataTypes = []string{nativeTypeBit, typeBoolean}
		case p.Family == modeMySQL && c.DDL == typeReal:
			c.MetadataTypes = []string{typeFloat, typeDouble}
		case p.Mode == modeA && c.DDL == typeDate:
			c.MetadataTypes = []string{typeDate, typeTimestamp}
		case p.Driver == driverDM && p.Mode == driverOracle && c.DDL == typeDate:
			c.MetadataTypes = []string{typeDate, typeTimestamp}
		case p.Family == driverDM && c.DDL == "NCLOB":
			c.MetadataTypes = []string{nativeTypeText}
		case p.Family == driverDM && c.DDL == "RAW(4)":
			c.MetadataTypes = []string{"VARBINARY"}
		case p.Family == driverDM && c.DDL == "TIMESTAMP(6) WITH TIME ZONE":
			c.MetadataTypes = []string{"DATETIME WITH TIME ZONE"}
		}
	}
}

func numericText(p profile) string {
	switch p.Family {
	case driverOracle:
		return "TO_CHAR(v)"
	case modeMySQL:
		return "CAST(v AS CHAR)"
	default:
		return "CAST(v AS VARCHAR(200))"
	}
}

func addLargeAndBinary(p profile, add addType) {
	bytes := make([]byte, 256)
	for i := range bytes {
		bytes[i] = byte(i)
	}
	allBytes := strings.ToUpper(hex.EncodeToString(bytes))
	insert, update, binaryType, read := "HEXTORAW('"+allBytes+"')", "HEXTORAW('FF00')", "VARBINARY(256)", "RAWTOHEX(v)"
	if p.Family == driverOracle {
		binaryType = "RAW(256)"
	}
	if p.Family == modeMySQL {
		insert = "X'" + allBytes + "'"
		update = "X'FF00'"
		read = "HEX(v)"
	}
	if p.Family == modePG {
		binaryType = "BYTEA"
		insert = "decode('" + allBytes + "','hex')"
		update = "decode('ff00','hex')"
		read = "upper(encode(v,'hex'))"
	}
	add("binary.all-bytes", binaryType, binaryType, insert, update, read, allBytes, "FF00")
	chunk := strings.Repeat("中文𐐷", 100)
	literal := "'" + strings.Repeat(chunk, 120) + "'"
	updatedLiteral := "'" + strings.Repeat("更新𐐷", 12000) + "'"
	lobType := nativeTypeText
	equalInitial, equalUpdated := "v="+literal, "v="+updatedLiteral
	if p.Family == modeMySQL {
		lobType = "LONGTEXT"
		equalInitial, equalUpdated = "BINARY v=BINARY "+literal, "BINARY v=BINARY "+updatedLiteral
	}
	if p.Family == driverOracle || p.Family == driverDM {
		lobType = typeClob
		initialChunks, updatedChunks := make([]string, 120), make([]string, 120)
		for i := range initialChunks {
			initialChunks[i] = "TO_CLOB('" + chunk + "')"
			updatedChunks[i] = "TO_CLOB('" + strings.Repeat("更新𐐷", 100) + "')"
		}
		literal, updatedLiteral = strings.Join(initialChunks, "||"), strings.Join(updatedChunks, "||")
		equalInitial, equalUpdated = "DBMS_LOB.COMPARE(v,"+literal+")=0", "DBMS_LOB.COMPARE(v,"+updatedLiteral+")=0"
	}
	add("lob.large-unicode", lobType, lobType, literal, updatedLiteral, "CASE WHEN "+equalInitial+" THEN 'exact-initial' WHEN "+equalUpdated+" THEN 'exact-updated' ELSE 'MISMATCH' END", "exact-initial", "exact-updated")
}

func typeID(name string) string {
	return strings.ToLower(strings.NewReplacer(" ", "-", "_", "-", "(", "-", ")", "", ",", "-").Replace(name))
}

func emptyText(p profile) string {
	if p.Family == driverOracle || (p.Family == driverDM && p.Mode == driverOracle) || p.Mode == modeA {
		return nullValue
	}
	return "<EMPTY>"
}

type addType func(id, name, ddl, insert, update, expression, expected, updated string)
type conditionalType func(id, name, reason string)

func pgTypes(cases *[]typeCase, add addType, conditional conditionalType) {
	add("numeric.numeric-alias", "NUMERIC(38,4)", "NUMERIC(38,4)", "1.2300", "-2.3400", "CAST(v AS VARCHAR(100))", "1.2300", "-2.3400")
	add("lob.text", nativeTypeText, nativeTypeText, "'中文 text'", "'更新 text'", "v", "中文 text", "更新 text")
	add("binary.zero-ff", "BYTEA", "BYTEA", "decode('00ff0100','hex')", "decode('ff0001ff','hex')", "upper(encode(v,'hex'))", "00FF0100", "FF0001FF")
	add("binary.empty-null", "BYTEA", "BYTEA", "decode('','hex')", sqlNull, "CASE WHEN v IS NULL THEN '<NULL>' ELSE CAST(octet_length(v) AS VARCHAR(20)) END", "0", nullValue)
	add("temporal.date", typeDate, typeDate, "DATE '2000-02-29'", "DATE '1969-12-31'", "to_char(v,'YYYY-MM-DD')", "2000-02-29", "1969-12-31")
	add("temporal.time", "TIME(6)", "TIME(6)", "TIME '23:59:59.123456'", "TIME '00:00:00.000001'", "to_char(v,'HH24:MI:SS.US')", "23:59:59.123456", "00:00:00.000001")
	add("temporal.timestamp", "TIMESTAMP(6)", "TIMESTAMP(6)", "TIMESTAMP '2000-02-29 23:59:59.123456'", "TIMESTAMP '1969-12-31 00:00:00.000001'", "to_char(v,'YYYY-MM-DD HH24:MI:SS.US')", "2000-02-29 23:59:59.123456", "1969-12-31 00:00:00.000001")
	add("timezone.timestamp", "TIMESTAMP(6) WITH TIME ZONE", "TIMESTAMP(6) WITH TIME ZONE", "TIMESTAMP WITH TIME ZONE '2000-02-29 23:59:59.123456+08:00'", "TIMESTAMP WITH TIME ZONE '1969-12-31 00:00:00.000001-03:00'", "to_char(v AT TIME ZONE 'UTC','YYYY-MM-DD HH24:MI:SS.US')", "2000-02-29 15:59:59.123456", "1969-12-31 03:00:00.000001")
	add("timezone.time", "TIME(6) WITH TIME ZONE", "TIME(6) WITH TIME ZONE", "TIME WITH TIME ZONE '23:59:59.123456+08:00'", "TIME WITH TIME ZONE '00:00:00.000001+00:00'", "CASE WHEN v=TIME WITH TIME ZONE '23:59:59.123456+08:00' THEN 'offset-8' WHEN v=TIME WITH TIME ZONE '00:00:00.000001+00:00' THEN 'utc' ELSE 'MISMATCH' END", "offset-8", "utc")
	add("interval.native", "INTERVAL", "INTERVAL", "INTERVAL '-1 day 02:03:04.123456'", "INTERVAL '0 seconds'", "CASE WHEN v=INTERVAL '-1 day 02:03:04.123456' THEN 'negative-fractional' WHEN v=INTERVAL '0 seconds' THEN 'zero' ELSE 'MISMATCH' END", "negative-fractional", "zero")
	add("structured.json", "JSON", "JSON", "'{\"n\":123,\"s\":\"中文\"}'", "'{\"n\":456,\"s\":null}'", "CAST(v->>'n' AS VARCHAR(10))", "123", "456")
	add("structured.jsonb", "JSONB", "JSONB", "'{\"a\":1,\"b\":2}'", "'{\"b\":3,\"a\":1}'", "CASE WHEN v='{\"b\":2,\"a\":1}'::jsonb THEN 'equal-2' WHEN v='{\"a\":1,\"b\":3}'::jsonb THEN 'equal-3' ELSE 'MISMATCH' END", "equal-2", "equal-3")
	add("structured.xml", "XML", "XML", "XMLPARSE(DOCUMENT '<r>中文</r>')", "XMLPARSE(DOCUMENT '<r>updated</r>')", "XMLSERIALIZE(DOCUMENT v AS TEXT)", "<r>中文</r>", "<r>updated</r>")
	add("structured.uuid", "UUID", "UUID", "'00112233-4455-6677-8899-aabbccddeeff'", "'ffffffff-ffff-ffff-ffff-ffffffffffff'", "CAST(v AS VARCHAR(36))", "00112233-4455-6677-8899-aabbccddeeff", "ffffffff-ffff-ffff-ffff-ffffffffffff")
	add("structured.array", "INTEGER[]", "INTEGER[]", "ARRAY[1,NULL,3]", "ARRAY[-1,0,4]", "CAST(v AS TEXT)", "{1,NULL,3}", "{-1,0,4}")
	add("structured.enum", "mp_native_enum", "mp_native_enum", "'first'", "'second'", "CAST(v AS TEXT)", "first", "second")
	(*cases)[len(*cases)-1].Setup = "CREATE TYPE mp_native_enum AS ENUM ('first','second');"
	(*cases)[len(*cases)-1].Teardown = "DROP TYPE mp_native_enum;"
	add("structured.domain", "mp_native_domain", "mp_native_domain", "1", "2", "CAST(v AS TEXT)", "1", "2")
	(*cases)[len(*cases)-1].Setup = "CREATE DOMAIN mp_native_domain AS INTEGER CHECK(VALUE>0);"
	(*cases)[len(*cases)-1].Teardown = "DROP DOMAIN mp_native_domain;"
	add("structured.composite", "mp_native_type", "mp_native_type", "ROW(1,'first')::mp_native_type", "ROW(2,'second')::mp_native_type", "CAST((v).n AS TEXT)||':'||(v).s", "1:first", "2:second")
	(*cases)[len(*cases)-1].Setup = "CREATE TYPE mp_native_type AS (n INTEGER,s VARCHAR(20));"
	(*cases)[len(*cases)-1].Teardown = dropNativeType
	for _, name := range []string{"BINARY", "VARBINARY", nativeTypeBlob, typeClob, typeNVarchar2, "MONEY", "HSTORE", nativeTypeGeometry, "GEOGRAPHY", "TSVECTOR", "TSQUERY", "OID", "NAME", "CIDR", "INET", "MACADDR", nativeTypeBit, "BIT VARYING"} {
		conditional("structured.native-"+typeID(name), name, "Requires declared native product/version or installed extension catalog and a precise scalar/DDL contract")
	}
}

func mysqlTypes(_ *[]typeCase, add addType, conditional conditionalType) {
	for _, width := range []struct{ name, min, max string }{
		{"TINYINT", "-128", "127"}, {"MEDIUMINT", "-8388608", "8388607"},
		{"TINYINT UNSIGNED", "0", "255"}, {"SMALLINT UNSIGNED", "0", "65535"}, {"MEDIUMINT UNSIGNED", "0", "16777215"}, {"INT UNSIGNED", "0", "4294967295"}, {"BIGINT UNSIGNED", "0", "18446744073709551615"},
	} {
		add("integer."+typeID(width.name), width.name, width.name, width.min, width.max, "CAST(v AS CHAR)", width.min, width.max)
	}
	add("numeric.precision-65", "DECIMAL(65,4)", "DECIMAL(65,4)", "1234567890123456789012345678901234567890123456789012345678901.2300", "-1234567890123456789012345678901234567890123456789012345678901.2300", "CAST(v AS CHAR)", "1234567890123456789012345678901234567890123456789012345678901.2300", "-1234567890123456789012345678901234567890123456789012345678901.2300")
	add("boolean.boolean-alias", typeBoolean, typeBoolean, "FALSE", "TRUE", "CASE WHEN v=1 THEN 'true' ELSE 'false' END", "false", "true")
	add("boolean.bit-eight", "BIT(8)", "BIT(8)", "b'00000000'", "b'11111111'", "CAST(v+0 AS CHAR)", "0", "255")
	for _, name := range []string{"TINYTEXT", nativeTypeText, "MEDIUMTEXT", "LONGTEXT"} {
		add("lob."+typeID(name), name, name, "'中文 LOB'", "'更新 LOB'", "v", "中文 LOB", "更新 LOB")
	}
	for _, name := range []string{"BINARY(4)", "VARBINARY(4)", "TINYBLOB", nativeTypeBlob, "MEDIUMBLOB", "LONGBLOB"} {
		add("binary."+typeID(name), name, name, "X'00FF0100'", "X'FF0001FF'", "HEX(v)", "00FF0100", "FF0001FF")
	}
	add("binary.zero-ff", "VARBINARY(4)", "VARBINARY(4)", "X'00FF0100'", "X'FF0001FF'", "HEX(v)", "00FF0100", "FF0001FF")
	add("binary.empty-null", "VARBINARY(4)", "VARBINARY(4)", "X''", sqlNull, "CASE WHEN v IS NULL THEN '<NULL>' ELSE CAST(OCTET_LENGTH(v) AS CHAR) END", "0", nullValue)
	add("temporal.date", typeDate, typeDate, "'2000-02-29'", "'1969-12-31'", "DATE_FORMAT(v,'%Y-%m-%d')", "2000-02-29", "1969-12-31")
	add("temporal.time", "TIME(6)", "TIME(6)", "'-12:34:56.123456'", "'00:00:00.000001'", "CAST(v AS CHAR)", "-12:34:56.123456", "00:00:00.000001")
	add("temporal.datetime", "DATETIME(6)", "DATETIME(6)", "'2000-02-29 23:59:59.123456'", "'1969-12-31 00:00:00.000001'", "DATE_FORMAT(v,'%Y-%m-%d %H:%i:%s.%f')", "2000-02-29 23:59:59.123456", "1969-12-31 00:00:00.000001")
	add("temporal.year", "YEAR", "YEAR", "1901", "2155", "CAST(v AS CHAR)", "1901", "2155")
	add("timezone.timestamp", "TIMESTAMP(6)", "TIMESTAMP(6)", "'2000-02-29 23:59:59.123456'", "'2001-01-01 00:00:00.000001'", "DATE_FORMAT(v,'%Y-%m-%d %H:%i:%s.%f')", "2000-02-29 23:59:59.123456", "2001-01-01 00:00:00.000001")
	conditional("timezone.timestamp-utc", "TIMESTAMP UTC instant", "Requires controlled migration and independent observer session time_zone values; wall-clock readback alone cannot verify UTC conversion")
	conditional("interval.native", "INTERVAL column", "MySQL-family INTERVAL is an expression unit, not a native storable column type; no column migration applies")
	add("structured.json", "JSON", "JSON", "'{\"n\":123,\"s\":\"中文\"}'", "'{\"n\":456,\"s\":null}'", "JSON_UNQUOTE(JSON_EXTRACT(v,'$.n'))", "123", "456")
	add("structured.enum", "ENUM", "ENUM('first','second')", "'first'", "'second'", "CAST(v AS CHAR)", "first", "second")
	add("structured.set", "SET", "SET('a','b','c')", "'b,a'", "'c,a'", "CAST(v AS CHAR)", "a,b", "a,c")
	for _, name := range []string{nativeTypeGeometry, "POINT", "LINESTRING", "POLYGON", "MULTIPOINT", "MULTILINESTRING", "MULTIPOLYGON", "GEOMETRYCOLLECTION"} {
		conditional("structured."+typeID(name), name, "OceanBase spatial-type availability and native canonical readback require the declared server version's feature catalog")
	}
}

func oracleTypes(cases *[]typeCase, add addType, conditional conditionalType, p profile) {
	for _, name := range []string{"NCHAR(4)", "NVARCHAR2(40)"} {
		add("text."+typeID(name), name, name, "N'中文'", "N'更新'", "TRIM(v)", "中文", "更新")
	}
	add("binary.zero-ff", "RAW(4)", "RAW(4)", "HEXTORAW('00FF0100')", "HEXTORAW('FF0001FF')", "RAWTOHEX(v)", "00FF0100", "FF0001FF")
	add("binary.raw-null", "RAW(4)", "RAW(4)", sqlNull, "HEXTORAW('00')", "COALESCE(RAWTOHEX(v),'<NULL>')", nullValue, "00")
	add("binary.blob", nativeTypeBlob, nativeTypeBlob, "TO_BLOB(HEXTORAW('00FF0100'))", "TO_BLOB(HEXTORAW('FF0001FF'))", "RAWTOHEX(DBMS_LOB.SUBSTR(v,4,1))", "00FF0100", "FF0001FF")
	add("lob.clob", typeClob, typeClob, "TO_CLOB('中文 LOB')", "TO_CLOB('更新 LOB')", "DBMS_LOB.SUBSTR(v,100,1)", "中文 LOB", "更新 LOB")
	add("lob.nclob", "NCLOB", "NCLOB", "TO_NCLOB(N'中文 LOB')", "TO_NCLOB(N'更新 LOB')", "DBMS_LOB.SUBSTR(v,100,1)", "中文 LOB", "更新 LOB")
	add("temporal.date", typeDate, typeDate, "TO_DATE('2000-02-29 23:59:59','YYYY-MM-DD HH24:MI:SS')", "TO_DATE('1969-12-31 00:00:01','YYYY-MM-DD HH24:MI:SS')", "TO_CHAR(v,'YYYY-MM-DD HH24:MI:SS')", "2000-02-29 23:59:59", "1969-12-31 00:00:01")
	for _, precision := range []string{"0", "3", "6", "9"} {
		fraction := strings.Repeat("1", int(precision[0]-'0'))
		suffix := ""
		format := "YYYY-MM-DD HH24:MI:SS"
		if fraction != "" {
			suffix = "." + fraction
			format += ".FF" + precision
		}
		add("temporal.timestamp-"+precision, "TIMESTAMP("+precision+")", "TIMESTAMP("+precision+")", "TIMESTAMP '2000-02-29 23:59:59"+suffix+"'", "TIMESTAMP '1969-12-31 00:00:00"+suffix+"'", "TO_CHAR(v,'"+format+"')", "2000-02-29 23:59:59"+suffix, "1969-12-31 00:00:00"+suffix)
	}
	add("timezone.timestamp", "TIMESTAMP(6) WITH TIME ZONE", "TIMESTAMP(6) WITH TIME ZONE", "TO_TIMESTAMP_TZ('2000-02-29 23:59:59.123456 +08:00','YYYY-MM-DD HH24:MI:SS.FF6 TZH:TZM')", "TO_TIMESTAMP_TZ('1969-12-31 00:00:00.000001 -03:00','YYYY-MM-DD HH24:MI:SS.FF6 TZH:TZM')", "TO_CHAR(SYS_EXTRACT_UTC(v),'YYYY-MM-DD HH24:MI:SS.FF6')", "2000-02-29 15:59:59.123456", "1969-12-31 03:00:00.000001")
	conditional("timezone.local-timestamp", "TIMESTAMP WITH LOCAL TIME ZONE", "Requires explicit controlled migration/observer time zones and native DBTIMEZONE contract")
	add("interval.year-month", "INTERVAL YEAR(4) TO MONTH", "INTERVAL YEAR(4) TO MONTH", "INTERVAL '-1234-11' YEAR(4) TO MONTH", "INTERVAL '0-0' YEAR TO MONTH", "CASE WHEN v=INTERVAL '-1234-11' YEAR(4) TO MONTH THEN 'negative-1234-11' WHEN v=INTERVAL '0-0' YEAR TO MONTH THEN 'zero' ELSE 'MISMATCH' END", "negative-1234-11", "zero")
	add("interval.day-second", "INTERVAL DAY(4) TO SECOND(6)", "INTERVAL DAY(4) TO SECOND(6)", "INTERVAL '-1234 02:03:04.123456' DAY(4) TO SECOND(6)", "INTERVAL '0 00:00:00' DAY TO SECOND", "CASE WHEN v=INTERVAL '-1234 02:03:04.123456' DAY(4) TO SECOND(6) THEN 'negative-fractional' WHEN v=INTERVAL '0 00:00:00' DAY TO SECOND THEN 'zero' ELSE 'MISMATCH' END", "negative-fractional", "zero")
	add("structured.object", "mp_native_type", "mp_native_type", "mp_native_type(1,'first')", "mp_native_type(2,'second')", "'placeholder'", "1:first", "2:second")
	last := &(*cases)[len(*cases)-1]
	last.Setup = "CREATE TYPE mp_native_type AS OBJECT(n NUMBER(10),s VARCHAR2(20));\n/\n"
	last.Teardown = dropNativeType
	last.Query = "SELECT TO_CHAR(t.v.n)||':'||t.v.s FROM mp_type t WHERE id=1"
	add("structured.collection", "mp_native_type", "mp_native_type", "mp_native_type(1,2)", "mp_native_type(3)", "'placeholder'", "2", "1")
	last = &(*cases)[len(*cases)-1]
	last.Setup = "CREATE TYPE mp_native_type AS VARRAY(10) OF NUMBER(10);\n/\n"
	last.Teardown = dropNativeType
	last.Query = "SELECT TO_CHAR(COUNT(*)) FROM mp_type t,TABLE(t.v) n WHERE t.id=1"
	for _, item := range []struct{ id, name, reason string }{
		{"long", "LONG", "Legacy LONG cannot use ordinary scalar SQL expressions for exact observer readback; requires native streaming observer and one-LONG-column restrictions"},
		{"long-raw", "LONG RAW", "Legacy LONG RAW requires native streaming byte observation and restricted column operations"},
		{"bfile", "BFILE", "Requires explicitly configured server DIRECTORY and an external file fixture with read privileges; no external filesystem is assumed"},
		{"rowid", "ROWID", "Requires separately owned referenced heap row and preservation/invalid-reference semantics"},
		{"urowid", "UROWID", "Requires index-organized or native logical-rowid prerequisite and exact version contract"},
		{"json", "JSON", "Requires exact server-version native JSON datatype contract; CLOB containing JSON does not establish native JSON coverage"},
		{"xmltype", "XMLTYPE", "Requires installed native XML support and declared version contract; ordinary CLOB/XML text is not XMLTYPE"},
		{"spatial", "SDO_GEOMETRY", "Requires installed/licensed native spatial feature and version-specific object dependency contract"},
	} {
		conditional("structured."+item.id, item.name, item.reason)
	}
	if p.Driver == driverOceanBase {
		conditional("structured.oracle-feature-catalog", "Oracle-mode object feature catalog", "OceanBase Oracle compatibility does not imply Oracle feature availability; exact version/edition catalog remains unresolved")
	}
}

func dmTypes(add addType, conditional conditionalType, p profile) {
	for _, name := range []string{"NUMERIC(38,4)", "NUMBER(38,4)"} {
		add("numeric."+typeID(name), name, name, "1.2300", "-2.3400", "TO_CHAR(v,'FM9999999999999999999999999999999990.0000')", "1.2300", "-2.3400")
	}
	if p.Mode == modeNative || p.Mode == driverOracle {
		conditional("boolean.boolean-alias", typeBoolean, "DM8 native/Oracle modes reject SQL BOOLEAN with -6119; BIT is tested separately")
	} else {
		add("boolean.boolean-alias", typeBoolean, typeBoolean, "0", "1", "CASE WHEN v=1 THEN 'true' ELSE 'false' END", "false", "true")
	}
	add("binary.zero-ff", "VARBINARY(4)", "VARBINARY(4)", "HEXTORAW('00FF0100')", "HEXTORAW('FF0001FF')", "RAWTOHEX(v)", "00FF0100", "FF0001FF")
	add("binary.binary", "BINARY(4)", "BINARY(4)", "HEXTORAW('00FF0100')", "HEXTORAW('FF0001FF')", "RAWTOHEX(v)", "00FF0100", "FF0001FF")
	add("binary.blob", nativeTypeBlob, nativeTypeBlob, "HEXTORAW('00FF0100')", "HEXTORAW('FF0001FF')", "RAWTOHEX(DBMS_LOB.SUBSTR(v,4,1))", "00FF0100", "FF0001FF")
	add("lob.clob", typeClob, typeClob, "'中文 LOB'", "'更新 LOB'", "DBMS_LOB.SUBSTR(v,100,1)", "中文 LOB", "更新 LOB")
	add("temporal.date", typeDate, typeDate, "DATE '2000-02-29'", "DATE '1969-12-31'", "TO_CHAR(v,'YYYY-MM-DD')", "2000-02-29", "1969-12-31")
	add("temporal.time", "TIME(6)", "TIME(6)", "TIME '23:59:59.123456'", "TIME '00:00:00.000001'", "TO_CHAR(v,'HH24:MI:SS.FF6')", "23:59:59.123456", "00:00:00.000001")
	add("temporal.timestamp", "TIMESTAMP(6)", "TIMESTAMP(6)", "TIMESTAMP '2000-02-29 23:59:59.123456'", "TIMESTAMP '1969-12-31 00:00:00.000001'", "TO_CHAR(v,'YYYY-MM-DD HH24:MI:SS.FF6')", "2000-02-29 23:59:59.123456", "1969-12-31 00:00:00.000001")
	add("text.dm-varchar2", typeVarchar2, "VARCHAR2(40)", "'中文𐐷'", "'更新𐐷'", "v", "中文𐐷", "更新𐐷")
	add("text.dm-nchar", "NCHAR", "NCHAR(4)", "N'中文'", "N'更新'", "TRIM(v)", "中文", "更新")
	add("text.dm-nvarchar2", typeNVarchar2, "NVARCHAR2(40)", "N'中文𐐷'", "N'更新𐐷'", "v", "中文𐐷", "更新𐐷")
	add("lob.dm-nclob", "NCLOB", "NCLOB", "N'中文𐐷 LOB'", "N'更新𐐷 LOB'", "DBMS_LOB.SUBSTR(v,100,1)", "中文𐐷 LOB", "更新𐐷 LOB")
	add("binary.dm-raw", "RAW", "RAW(4)", "HEXTORAW('00FF0100')", "HEXTORAW('FF0001FF')", "RAWTOHEX(v)", "00FF0100", "FF0001FF")
	add("timezone.dm-timestamp-with-time-zone", "TIMESTAMP WITH TIME ZONE", "TIMESTAMP(6) WITH TIME ZONE", "TO_TIMESTAMP_TZ('2000-02-29 23:59:59.123456 +08:00','YYYY-MM-DD HH24:MI:SS.FF6 TZH:TZM')", "TO_TIMESTAMP_TZ('1969-12-31 00:00:00.000001 -03:00','YYYY-MM-DD HH24:MI:SS.FF6 TZH:TZM')", "TO_CHAR(SYS_EXTRACT_UTC(v),'YYYY-MM-DD HH24:MI:SS.FF6')", "2000-02-29 15:59:59.123456", "1969-12-31 03:00:00.000001")
	add("interval.dm-interval-year-to-month", "INTERVAL YEAR TO MONTH", "INTERVAL YEAR(4) TO MONTH", "INTERVAL '-1234-11' YEAR(4) TO MONTH", "INTERVAL '0-0' YEAR TO MONTH", "CASE WHEN v=INTERVAL '-1234-11' YEAR(4) TO MONTH THEN 'negative-1234-11' WHEN v=INTERVAL '0-0' YEAR TO MONTH THEN 'zero' ELSE 'MISMATCH' END", "negative-1234-11", "zero")
	add("interval.dm-interval-day-to-second", "INTERVAL DAY TO SECOND", "INTERVAL DAY(4) TO SECOND(6)", "INTERVAL '-1234 02:03:04.123456' DAY(4) TO SECOND(6)", "INTERVAL '0 00:00:00' DAY TO SECOND", "CASE WHEN v=INTERVAL '-1234 02:03:04.123456' DAY(4) TO SECOND(6) THEN 'negative-fractional' WHEN v=INTERVAL '0 00:00:00' DAY TO SECOND THEN 'zero' ELSE 'MISMATCH' END", "negative-fractional", "zero")
	for _, item := range []struct{ family, name string }{
		{"binary", "LONG RAW"}, {"timezone", "TIMESTAMP WITH LOCAL TIME ZONE"}, {nativeTypeStructured, "JSON"}, {nativeTypeStructured, "JSONB"}, {nativeTypeStructured, "XMLTYPE"}, {nativeTypeStructured, "UUID"}, {nativeTypeStructured, "ARRAY"}, {nativeTypeStructured, "OBJECT"}, {nativeTypeStructured, "CLASS"}, {nativeTypeStructured, "ENUM"}, {nativeTypeStructured, "DOMAIN"}, {nativeTypeStructured, nativeTypeGeometry},
	} {
		conditional(item.family+".dm-"+typeID(item.name), item.name, "Requires DM native catalog/version support in server mode "+p.Mode+"; no corresponding emulated-product syntax is inferred")
	}
}
