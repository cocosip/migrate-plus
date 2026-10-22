package oracle

import (
	"database/sql"
	"errors"
	"fmt"
)

// Drop removes supported objects in the original migration schema. It does not
// remove the Oracle user, grants, scheduler jobs, or database links.
func (o *Oracle) Drop() error {
	if err := o.checkExternalDependencies(); err != nil {
		return err
	}
	types, err := o.typeDropOrder()
	if err != nil {
		return err
	}
	if err := o.dropForeignKeys(); err != nil {
		return err
	}
	// Dropping a prebuilt materialized view leaves its table for the second query.
	for _, objectTypes := range []string{
		"'MATERIALIZED VIEW'",
		"'TRIGGER', 'VIEW', 'TABLE', 'SEQUENCE', 'SYNONYM', 'PACKAGE', 'PROCEDURE', 'FUNCTION'",
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
	for _, name := range types {
		if err := o.exec(`DROP TYPE ` + quoteIdentifier(o.schema) + `.` + quoteIdentifier(name)); err != nil {
			return err
		}
	}
	return nil
}

func (o *Oracle) typeDropOrder() (_ []string, err error) {
	ctx, cancel := o.operationContext()
	defer cancel()
	query := `SELECT t.object_name, d.referenced_name FROM ALL_OBJECTS t LEFT JOIN ALL_DEPENDENCIES d
ON d.owner = t.owner AND d.name = t.object_name AND d.type = 'TYPE'
AND d.referenced_owner = t.owner AND d.referenced_type = 'TYPE' AND d.referenced_link_name IS NULL
WHERE t.owner = :1 AND t.object_type = 'TYPE' AND t.subobject_name IS NULL
AND t.generated = 'N' AND t.secondary = 'N' AND t.object_name NOT LIKE 'BIN$%'
ORDER BY t.object_name, d.referenced_name`
	rows, err := o.conn.QueryContext(ctx, query, o.schema)
	if err != nil {
		return nil, queryError(query, err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var names []string
	remaining := make(map[string]bool)
	dependents := make(map[string][]string)
	for rows.Next() {
		var name string
		var referenced sql.NullString
		if err := rows.Scan(&name, &referenced); err != nil {
			return nil, err
		}
		if !remaining[name] {
			names = append(names, name)
			remaining[name] = true
		}
		if referenced.Valid && referenced.String != name {
			dependents[referenced.String] = append(dependents[referenced.String], name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, queryError(query, err)
	}
	var ordered []string
	for len(remaining) > 0 {
		before := len(remaining)
		for _, name := range names {
			if !remaining[name] {
				continue
			}
			blocked := false
			for _, dependent := range dependents[name] {
				blocked = blocked || remaining[dependent]
			}
			if !blocked {
				ordered = append(ordered, name)
				delete(remaining, name)
			}
		}
		if len(remaining) == before {
			return nil, fmt.Errorf("oracle: cyclic type dependencies; remove them explicitly before Drop")
		}
	}
	return ordered, nil
}

func (o *Oracle) dropStatements(objectTypes string) (_ []string, err error) {
	ctx, cancel := o.operationContext()
	defer cancel()
	query := `SELECT object_type, object_name FROM ALL_OBJECTS
WHERE owner = :1 AND subobject_name IS NULL AND generated = 'N' AND secondary = 'N'
AND object_type IN (` + objectTypes + `)
AND object_name NOT IN (SELECT table_name FROM ALL_NESTED_TABLES WHERE owner = :2)
AND object_name NOT LIKE 'BIN$%'
	ORDER BY DECODE(object_type, 'TRIGGER', 0, 'MATERIALIZED VIEW', 1, 'VIEW', 2, 'TABLE', 3, 'SEQUENCE', 4, 'SYNONYM', 5, 'PACKAGE', 6, 'PROCEDURE', 7, 'FUNCTION', 8, 'TYPE', 9)`
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
		return " PURGE", nil
	case "TRIGGER", "TYPE", "MATERIALIZED VIEW", "VIEW", "SEQUENCE", "SYNONYM", "PACKAGE", objectProcedure, objectFunction:
		return "", nil
	default:
		return "", fmt.Errorf("oracle: unsupported object type %q", kind)
	}
}

func (o *Oracle) checkExternalDependencies() error {
	ctx, cancel := o.operationContext()
	defer cancel()
	query := `SELECT COUNT(*) FROM (
SELECT 1 FROM ALL_CONSTRAINTS child JOIN ALL_CONSTRAINTS parent
ON parent.owner = child.r_owner AND parent.constraint_name = child.r_constraint_name
WHERE child.constraint_type = 'R' AND parent.owner = :1 AND child.owner <> :2
UNION ALL
SELECT 1 FROM ALL_DEPENDENCIES WHERE referenced_owner = :3 AND owner <> :4
AND referenced_link_name IS NULL)`
	var count int
	if err := o.conn.QueryRowContext(ctx, query, o.schema, o.schema, o.schema, o.schema).Scan(&count); err != nil {
		return queryError(query, err)
	}
	if count != 0 {
		return fmt.Errorf("oracle: Drop refuses dependencies from other schemas; remove them explicitly first")
	}
	return nil
}

func (o *Oracle) dropForeignKeys() (err error) {
	ctx, cancel := o.operationContext()
	defer cancel()
	query := `SELECT table_name, constraint_name FROM ALL_CONSTRAINTS WHERE owner = :1 AND constraint_type = 'R'`
	rows, err := o.conn.QueryContext(ctx, query, o.schema)
	if err != nil {
		return queryError(query, err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var statements []string
	for rows.Next() {
		var table, constraint string
		if err := rows.Scan(&table, &constraint); err != nil {
			return err
		}
		statements = append(statements, `ALTER TABLE `+quoteIdentifier(o.schema)+`.`+quoteIdentifier(table)+` DROP CONSTRAINT `+quoteIdentifier(constraint))
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, statement := range statements {
		if err := o.exec(statement); err != nil {
			return err
		}
	}
	return nil
}
