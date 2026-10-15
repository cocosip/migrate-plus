package oracle

import (
	"errors"
	"fmt"
)

// Drop removes supported objects in the original migration schema. It does not
// remove the Oracle user, grants, scheduler jobs, or database links.
func (o *Oracle) Drop() error {
	// Dropping a prebuilt materialized view leaves its table for the second query.
	for _, objectTypes := range []string{
		"'MATERIALIZED VIEW'",
		"'VIEW', 'TABLE', 'SEQUENCE', 'SYNONYM', 'PACKAGE', 'PROCEDURE', 'FUNCTION', 'TYPE'",
	} {
		objects, err := o.dropStatements(objectTypes)
		if err != nil {
			return err
		}
		for _, statement := range objects {
			if err := o.exec(statement); err != nil {
				return err
			}
		}
	}
	return nil
}

func (o *Oracle) dropStatements(objectTypes string) (_ []string, err error) {
	ctx, cancel := o.operationContext()
	defer cancel()
	query := `SELECT object_type, object_name FROM ALL_OBJECTS
WHERE owner = :1 AND subobject_name IS NULL AND generated = 'N' AND secondary = 'N'
AND object_type IN (` + objectTypes + `)
AND object_name NOT IN (SELECT table_name FROM ALL_NESTED_TABLES WHERE owner = :2)
AND object_name NOT LIKE 'BIN$%'
ORDER BY DECODE(object_type, 'MATERIALIZED VIEW', 0, 'VIEW', 1, 'TABLE', 2, 'SEQUENCE', 3, 'SYNONYM', 4, 'PACKAGE', 5, 'PROCEDURE', 6, 'FUNCTION', 7, 'TYPE', 8)`
	rows, err := o.conn.QueryContext(ctx, query, o.schema, o.schema)
	if err != nil {
		return nil, queryError(query, err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var statements []string
	for rows.Next() {
		var kind, name string
		if err := rows.Scan(&kind, &name); err != nil {
			return nil, err
		}
		suffix, err := dropSuffix(kind)
		if err != nil {
			return nil, err
		}
		statements = append(statements, `DROP `+kind+` `+quoteIdentifier(o.schema)+`.`+quoteIdentifier(name)+suffix)
	}
	if err := rows.Err(); err != nil {
		return nil, queryError(query, err)
	}
	return statements, nil
}

func dropSuffix(kind string) (string, error) {
	switch kind {
	case "TABLE":
		return " CASCADE CONSTRAINTS PURGE", nil
	case "TYPE":
		return " FORCE", nil
	case "MATERIALIZED VIEW", "VIEW", "SEQUENCE", "SYNONYM", "PACKAGE", objectProcedure, objectFunction:
		return "", nil
	default:
		return "", fmt.Errorf("oracle: unsupported object type %q", kind)
	}
}
