package oceanbase

import (
	"context"
	"database/sql"
	"fmt"

	ob "github.com/cocosip/go-oceanbase"
)

type dropObject struct{ name, kind string }
type dropDependency struct{ object, reference dropObject }

// Drop preflights visible dependencies and removes supported objects in the original schema.
// It never drops a tenant, database or user and never disables foreign-key enforcement.
func (d *OceanBase) Drop() error {
	ctx, cancel := d.operationContext()
	defer cancel()
	if err := d.checkSession(ctx); err != nil {
		return err
	}
	if err := d.checkExternalDependencies(ctx); err != nil {
		return err
	}
	objects, err := d.dropObjects(ctx)
	if err != nil {
		return err
	}
	if d.mode == ob.ModeOracle {
		deps, err := d.dropDependencies(ctx)
		if err != nil {
			return err
		}
		objects, err = orderDrop(objects, deps)
		if err != nil {
			return err
		}
	}
	keys, err := d.foreignKeys(ctx)
	if err != nil {
		return err
	}
	for _, key := range keys {
		clause := " DROP FOREIGN KEY "
		if d.mode == ob.ModeOracle {
			clause = " DROP CONSTRAINT "
		}
		if err := d.execContext(ctx, "ALTER TABLE "+d.qualified(key.name)+clause+d.quoteIdentifier(key.kind)); err != nil {
			return err
		}
	}
	for _, object := range objects {
		suffix := ""
		if d.mode == ob.ModeOracle && object.kind == "TABLE" {
			suffix = " PURGE"
		}
		if err := d.execContext(ctx, "DROP "+object.kind+" "+d.qualified(object.name)+suffix); err != nil {
			return err
		}
	}
	return nil
}

func (d *OceanBase) checkExternalDependencies(ctx context.Context) error {
	query := `SELECT COUNT(*) FROM (
SELECT 1 FROM information_schema.referential_constraints WHERE unique_constraint_schema = ? AND constraint_schema <> ?
UNION ALL
SELECT 1 FROM information_schema.view_table_usage WHERE table_schema = ? AND view_schema <> ?
) dependencies`
	if d.mode == ob.ModeOracle {
		query = `SELECT COUNT(*) FROM (
SELECT 1 FROM ALL_CONSTRAINTS child JOIN ALL_CONSTRAINTS parent
ON parent.owner = child.r_owner AND parent.constraint_name = child.r_constraint_name
WHERE child.constraint_type = 'R' AND parent.owner = :1 AND child.owner <> :2
UNION ALL
SELECT 1 FROM ALL_DEPENDENCIES WHERE referenced_owner = :3 AND owner <> :4
AND referenced_link_name IS NULL) dependencies`
	}
	var count int
	if err := d.operationError(ctx, query, d.conn.QueryRowContext(ctx, query, d.schema, d.schema, d.schema, d.schema).Scan(&count)); err != nil {
		return err
	}
	if count != 0 {
		return fmt.Errorf("oceanbase: Drop refuses visible dependencies from other schemas")
	}
	return nil
}

func (d *OceanBase) dropObjects(ctx context.Context) ([]dropObject, error) {
	query := `SELECT table_name, CASE table_type WHEN 'BASE TABLE' THEN 'TABLE' WHEN 'VIEW' THEN 'VIEW' ELSE 'UNSUPPORTED' END AS kind
FROM information_schema.tables WHERE table_schema = ?
UNION ALL SELECT routine_name, routine_type FROM information_schema.routines WHERE routine_schema = ?
UNION ALL SELECT trigger_name, 'TRIGGER' FROM information_schema.triggers WHERE trigger_schema = ?
UNION ALL SELECT event_name, 'EVENT' FROM information_schema.events WHERE event_schema = ?
ORDER BY kind DESC, table_name`
	args := []any{d.schema, d.schema, d.schema, d.schema}
	if d.mode == ob.ModeOracle {
		query = `SELECT object_name, object_type FROM ALL_OBJECTS WHERE owner = :1 AND subobject_name IS NULL
AND generated = 'N' AND secondary = 'N' AND object_type NOT IN ('INDEX', 'LOB', 'PACKAGE BODY', 'TYPE BODY')
AND object_name NOT LIKE 'BIN$%' ORDER BY DECODE(object_type, 'TRIGGER', 0, 'MATERIALIZED VIEW', 1, 'VIEW', 2, 'TABLE', 3, 'TYPE', 9, 5), object_name`
		args = []any{d.schema}
	}
	var objects []dropObject
	err := d.query(ctx, query, func(rows *sql.Rows) error {
		for rows.Next() {
			var object dropObject
			if err := rows.Scan(&object.name, &object.kind); err != nil {
				return err
			}
			switch object.kind {
			case "TABLE", "VIEW", objectProcedure, objectFunction, objectTrigger:
			case "SEQUENCE", "SYNONYM", "PACKAGE", "TYPE":
				if d.mode != ob.ModeOracle {
					return fmt.Errorf("oceanbase: unsupported MySQL Drop object %q", object.kind)
				}
			default:
				return fmt.Errorf("oceanbase: unsupported Drop object %q; remove it explicitly first", object.kind)
			}
			objects = append(objects, object)
		}
		return nil
	}, args...)
	return objects, err
}

func (d *OceanBase) foreignKeys(ctx context.Context) ([]dropObject, error) {
	query := `SELECT table_name, constraint_name FROM information_schema.table_constraints WHERE constraint_schema = ? AND constraint_type = 'FOREIGN KEY'`
	if d.mode == ob.ModeOracle {
		query = `SELECT table_name, constraint_name FROM ALL_CONSTRAINTS WHERE owner = :1 AND constraint_type = 'R'`
	}
	var keys []dropObject
	err := d.query(ctx, query, func(rows *sql.Rows) error {
		for rows.Next() {
			var key dropObject
			if err := rows.Scan(&key.name, &key.kind); err != nil {
				return err
			}
			keys = append(keys, key)
		}
		return nil
	}, d.schema)
	return keys, err
}

func (d *OceanBase) dropDependencies(ctx context.Context) ([]dropDependency, error) {
	query := `SELECT name, type, referenced_name, referenced_type FROM ALL_DEPENDENCIES
WHERE owner = :1 AND referenced_owner = :2 AND referenced_link_name IS NULL`
	var deps []dropDependency
	err := d.query(ctx, query, func(rows *sql.Rows) error {
		for rows.Next() {
			var dep dropDependency
			if err := rows.Scan(&dep.object.name, &dep.object.kind, &dep.reference.name, &dep.reference.kind); err != nil {
				return err
			}
			deps = append(deps, dep)
		}
		return nil
	}, d.schema, d.schema)
	return deps, err
}

func orderDrop(objects []dropObject, deps []dropDependency) ([]dropObject, error) {
	remaining := make(map[dropObject]bool, len(objects))
	for _, object := range objects {
		remaining[object] = true
	}
	var ordered []dropObject
	for len(remaining) > 0 {
		before := len(remaining)
		for _, object := range objects {
			if !remaining[object] {
				continue
			}
			blocked := false
			for _, dep := range deps {
				if dep.reference == object && dep.object != object && remaining[dep.object] {
					blocked = true
					break
				}
			}
			if !blocked {
				ordered = append(ordered, object)
				delete(remaining, object)
			}
		}
		if before == len(remaining) {
			return nil, fmt.Errorf("oceanbase: cyclic object dependencies; remove them explicitly before Drop")
		}
	}
	return ordered, nil
}
