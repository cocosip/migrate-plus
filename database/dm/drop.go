package dm

import (
	"database/sql"
	"fmt"
)

type dropObject struct {
	id   int64
	name string
	kind string
}

const (
	objectView      = "VIEW"
	objectBegin     = "BEGIN"
	objectProcedure = "PROCEDURE"
	objectFunction  = "FUNCTION"
	objectType      = "TYPE"
	objectClass     = "CLASS"
)

// Drop removes supported objects only in the original migration schema.
// External dependencies and unresolved cycles fail without using CASCADE.
func (d *Dm) Drop() error {
	if err := d.checkExternalDependencies(); err != nil {
		return err
	}
	if err := d.checkDropOrder(); err != nil {
		return err
	}
	if err := d.dropForeignKeys(); err != nil {
		return err
	}
	// Re-query after materialized views: DM removes their backing tables too.
	for _, materialized := range []int{1, 0} {
		objects, err := d.dropObjects(materialized)
		if err != nil {
			return err
		}
		dependencies, err := d.objectDependencies()
		if err != nil {
			return err
		}
		ordered, err := orderObjects(objects, dependencies)
		if err != nil {
			return err
		}
		for _, object := range ordered {
			suffix := ""
			switch object.kind {
			case "TABLE", objectView, objectType, objectClass:
				suffix = " RESTRICT"
			}
			if err := d.exec(`DROP ` + object.kind + ` ` + quoteIdentifier(d.schema) + `.` + quoteIdentifier(object.name) + suffix); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *Dm) checkDropOrder() error {
	var objects []dropObject
	for _, materialized := range []int{1, 0} {
		phase, err := d.dropObjects(materialized)
		if err != nil {
			return err
		}
		objects = append(objects, phase...)
	}
	dependencies, err := d.objectDependencies()
	if err != nil {
		return err
	}
	_, err = orderObjects(objects, dependencies)
	return err
}

func (d *Dm) checkExternalDependencies() error {
	query := `SELECT COUNT(*) FROM SYS.SYSDEPENDENCIES d
JOIN SYS.SYSOBJECTS dependent ON dependent.ID = d.ID
JOIN SYS.SYSOBJECTS referenced ON referenced.ID = d.REFED_ID
WHERE referenced.SCHID = ? AND dependent.SCHID <> ?`
	return d.query(query, func(rows *sql.Rows) error {
		var count int
		if !rows.Next() {
			return sql.ErrNoRows
		}
		if err := rows.Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("dm: Drop refuses dependencies from other schemas; remove them explicitly first")
		}
		return nil
	}, d.schemaID, d.schemaID)
}

func (d *Dm) dropForeignKeys() error {
	query := `SELECT t.NAME, c.NAME FROM SYS.SYSCONS f
JOIN SYS.SYSOBJECTS c ON c.ID = f.ID
JOIN SYS.SYSOBJECTS t ON t.ID = f.TABLEID
WHERE f.TYPE$ = 'F' AND t.SCHID = ?`
	var statements []string
	if err := d.query(query, func(rows *sql.Rows) error {
		for rows.Next() {
			var table, constraint string
			if err := rows.Scan(&table, &constraint); err != nil {
				return err
			}
			statements = append(statements, `ALTER TABLE `+quoteIdentifier(d.schema)+`.`+quoteIdentifier(table)+` DROP CONSTRAINT `+quoteIdentifier(constraint))
		}
		return nil
	}, d.schemaID); err != nil {
		return err
	}
	for _, statement := range statements {
		if err := d.exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func (d *Dm) dropObjects(materialized int) ([]dropObject, error) {
	mview := `CASE WHEN EXISTS (SELECT 1 FROM SYS.SYSDEPENDENCIES dep WHERE dep.ID = o.ID AND dep.TYPE$ = 'MATERIALIZED VIEW') THEN 1 ELSE 0 END`
	// INFO1 BIT(1) marks table-level triggers; DM drops them with their table,
	// so only standalone event triggers are selected by the PID-relaxed branch.
	query := `SELECT o.ID, o.NAME, o.SUBTYPE$, o.INFO1, ` + mview + ` FROM SYS.SYSOBJECTS o
WHERE o.SCHID = ? AND o.TYPE$ = 'SCHOBJ' AND (o.PID = -1 OR o.SUBTYPE$ IN ('VIEW', 'SEQ', 'SYNOM', 'PKG', 'PROC', 'TYPE', 'CLASS') OR (o.SUBTYPE$ = 'TRIG' AND BITAND(o.INFO1, 2) = 0))
AND o.SUBTYPE$ IN ('UTAB', 'VIEW', 'SEQ', 'SYNOM', 'PKG', 'PROC', 'TYPE', 'CLASS', 'TRIG')
AND NOT (o.SUBTYPE$ IN ('PKG', 'PROC') AND BITAND(o.INFO1, 4) <> 0)
AND NOT (o.SUBTYPE$ = 'VIEW' AND BITAND(o.INFO1, 16) <> 0)
AND NOT (o.SUBTYPE$ = 'TRIG' AND BITAND(o.INFO1, 32) <> 0)
AND ` + mview + ` = ? ORDER BY o.ID`
	var objects []dropObject
	err := d.query(query, func(rows *sql.Rows) error {
		for rows.Next() {
			var object dropObject
			var subtype string
			var info int64
			var mv int
			if err := rows.Scan(&object.id, &object.name, &subtype, &info, &mv); err != nil {
				return err
			}
			kind, err := dropKind(subtype, info, mv == 1)
			if err != nil {
				return err
			}
			object.kind = kind
			objects = append(objects, object)
		}
		return nil
	}, d.schemaID, materialized)
	return objects, err
}

func dropKind(subtype string, info int64, materialized bool) (string, error) {
	if materialized {
		if subtype != objectView {
			return "", fmt.Errorf("dm: unexpected materialized view object type")
		}
		return "MATERIALIZED VIEW", nil
	}
	switch subtype {
	case "UTAB":
		return "TABLE", nil
	case objectView, objectType:
		return subtype, nil
	case "SEQ":
		return "SEQUENCE", nil
	case "SYNOM":
		return "SYNONYM", nil
	case "PKG":
		return "PACKAGE", nil
	case "TRIG":
		return "TRIGGER", nil
	case objectClass:
		if info&64 != 0 {
			return objectType, nil
		}
		return objectClass, nil
	case "PROC":
		if info&1 != 0 {
			return objectProcedure, nil
		}
		return objectFunction, nil
	default:
		return "", fmt.Errorf("dm: unsupported Drop object type")
	}
}

func (d *Dm) objectDependencies() ([][2]int64, error) {
	query := `SELECT d.ID, d.REFED_ID FROM SYS.SYSDEPENDENCIES d
JOIN SYS.SYSOBJECTS dependent ON dependent.ID = d.ID
JOIN SYS.SYSOBJECTS referenced ON referenced.ID = d.REFED_ID
WHERE dependent.SCHID = ? AND referenced.SCHID = ?`
	var dependencies [][2]int64
	err := d.query(query, func(rows *sql.Rows) error {
		for rows.Next() {
			var pair [2]int64
			if err := rows.Scan(&pair[0], &pair[1]); err != nil {
				return err
			}
			dependencies = append(dependencies, pair)
		}
		return nil
	}, d.schemaID, d.schemaID)
	return dependencies, err
}

func orderObjects(objects []dropObject, dependencies [][2]int64) ([]dropObject, error) {
	remaining := make(map[int64]bool, len(objects))
	for _, object := range objects {
		remaining[object.id] = true
	}
	var ordered []dropObject
	for len(remaining) > 0 {
		before := len(remaining)
		for _, object := range objects {
			if !remaining[object.id] || hasDependent(object.id, dependencies, remaining) {
				continue
			}
			ordered = append(ordered, object)
			delete(remaining, object.id)
		}
		if len(remaining) == before {
			return nil, fmt.Errorf("dm: cyclic object dependencies; remove the cycle with explicit migration SQL before Drop")
		}
	}
	return ordered, nil
}

func hasDependent(id int64, dependencies [][2]int64, remaining map[int64]bool) bool {
	for _, edge := range dependencies {
		if edge[1] == id && edge[0] != id && remaining[edge[0]] {
			return true
		}
	}
	return false
}
