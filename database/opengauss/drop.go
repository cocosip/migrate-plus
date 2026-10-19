package opengauss

import (
	"database/sql"
	"fmt"

	opq "gitcode.com/opengauss/openGauss-connector-go-pq"
)

type objectID struct{ class, oid int64 }

type dropObject struct {
	id               objectID
	name, kind, args string
}

type dependency struct {
	object, reference objectID
	kind              string
	local, refLocal   bool
}

type foreignKey struct {
	id                objectID
	table, constraint string
}

func quoteQualified(schema, name string) string {
	return opq.QuoteIdentifier(schema) + "." + opq.QuoteIdentifier(name)
}

// Drop preflights schema dependencies and removes supported objects without CASCADE.
func (g *OpenGauss) Drop() error {
	if err := g.usable(); err != nil {
		return err
	}
	for {
		objects, err := g.dropObjects()
		if err != nil {
			return err
		}
		keys, err := g.foreignKeys()
		if err != nil {
			return err
		}
		deps, err := g.dropDependencies()
		if err != nil {
			return err
		}
		ordered, err := orderDrop(objects, keys, deps)
		if err != nil {
			return err
		}
		for _, key := range keys {
			if err := g.exec(`ALTER TABLE ` + quoteQualified(g.schema, key.table) + ` DROP CONSTRAINT ` + opq.QuoteIdentifier(key.constraint) + ` RESTRICT`); err != nil {
				return err
			}
		}
		lastMaterialized := -1
		for i, object := range ordered {
			if object.kind == "MATERIALIZED VIEW" {
				lastMaterialized = i
			}
		}
		for i, object := range ordered {
			name := quoteQualified(g.schema, object.name)
			if object.kind == "FUNCTION" || object.kind == "PROCEDURE" {
				name += "(" + object.args + ")"
			}
			if err := g.exec(`DROP ` + object.kind + ` ` + name + ` RESTRICT`); err != nil {
				return err
			}
			if i == lastMaterialized {
				break
			}
		}
		if lastMaterialized < 0 {
			return nil
		}
		// Materialized views can remove backing objects; resolve the remaining catalog again.
	}
}

func (g *OpenGauss) dropObjects() ([]dropObject, error) {
	query := `SELECT 'pg_catalog.pg_class'::regclass::oid::bigint, c.oid::bigint, c.relname,
CASE c.relkind WHEN 'r' THEN 'TABLE' WHEN 'p' THEN 'TABLE' WHEN 'v' THEN 'VIEW'
WHEN 'm' THEN 'MATERIALIZED VIEW' WHEN 'S' THEN 'SEQUENCE' WHEN 'L' THEN 'SEQUENCE' ELSE 'UNSUPPORTED' END, ''::text
FROM pg_catalog.pg_class c WHERE c.relnamespace = $1 AND c.relkind NOT IN ('i', 'I', 't', 'c')
AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d JOIN pg_catalog.pg_class owner ON owner.oid = d.refobjid
WHERE d.classid = 'pg_catalog.pg_class'::regclass AND d.objid = c.oid
AND d.refclassid = 'pg_catalog.pg_class'::regclass AND d.deptype IN ('i', 'a') AND owner.relnamespace = $1)
UNION ALL
SELECT 'pg_catalog.pg_proc'::regclass::oid::bigint, p.oid::bigint, p.proname,
CASE p.prokind WHEN 'f' THEN 'FUNCTION' WHEN 'p' THEN 'PROCEDURE' ELSE 'UNSUPPORTED' END,
pg_catalog.pg_get_function_identity_arguments(p.oid)
FROM pg_catalog.pg_proc p WHERE p.pronamespace = $1
UNION ALL
SELECT 'pg_catalog.pg_type'::regclass::oid::bigint, t.oid::bigint, t.typname,
CASE WHEN t.typtype = 'd' THEN 'DOMAIN' ELSE 'TYPE' END, ''::text
FROM pg_catalog.pg_type t LEFT JOIN pg_catalog.pg_class c ON c.oid = t.typrelid
WHERE t.typnamespace = $1 AND (t.typrelid = 0 OR c.relkind = 'c')
AND NOT (t.typelem <> 0 AND t.typlen = -1)
ORDER BY 1, 2`
	var objects []dropObject
	err := g.query(query, func(rows *sql.Rows) error {
		for rows.Next() {
			var object dropObject
			if err := rows.Scan(&object.id.class, &object.id.oid, &object.name, &object.kind, &object.args); err != nil {
				return err
			}
			if object.kind == "UNSUPPORTED" {
				return fmt.Errorf("opengauss: Drop encountered an unsupported relation or routine")
			}
			objects = append(objects, object)
		}
		return nil
	}, g.schemaID)
	return objects, err
}

func (g *OpenGauss) foreignKeys() ([]foreignKey, error) {
	query := `SELECT c.tableoid::bigint, c.oid::bigint, t.relname, c.conname FROM pg_catalog.pg_constraint c
JOIN pg_catalog.pg_class t ON t.oid = c.conrelid WHERE c.contype = 'f' AND t.relnamespace = $1`
	var keys []foreignKey
	err := g.query(query, func(rows *sql.Rows) error {
		for rows.Next() {
			var key foreignKey
			if err := rows.Scan(&key.id.class, &key.id.oid, &key.table, &key.constraint); err != nil {
				return err
			}
			keys = append(keys, key)
		}
		return nil
	}, g.schemaID)
	return keys, err
}

func (g *OpenGauss) dropDependencies() ([]dependency, error) {
	query := `WITH RECURSIVE scoped(classid, objid) AS (
SELECT d.classid, d.objid FROM pg_catalog.pg_depend d
WHERE d.refclassid = 'pg_catalog.pg_namespace'::regclass AND d.refobjid = $1
UNION
SELECT d.classid, d.objid FROM pg_catalog.pg_depend d JOIN scoped s
ON s.classid = d.refclassid AND s.objid = d.refobjid WHERE d.deptype IN ('i', 'a')
)
SELECT s.classid::bigint, s.objid::bigint, 0::bigint, 0::bigint, 's'::text, true, false FROM scoped s
UNION ALL
SELECT d.classid::bigint, d.objid::bigint, d.refclassid::bigint, d.refobjid::bigint,
CASE WHEN d.refclassid = 'pg_catalog.pg_namespace'::regclass AND d.refobjid <> $1
AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace n WHERE n.oid = d.refobjid
AND (n.nspname = 'pg_toast' OR pg_catalog.substr(n.nspname::text, 1, 9) = 'pg_toast_')) THEN 'x' ELSE d.deptype::text END,
EXISTS (SELECT 1 FROM scoped s WHERE s.classid = d.classid AND s.objid = d.objid),
EXISTS (SELECT 1 FROM scoped s WHERE s.classid = d.refclassid AND s.objid = d.refobjid)
FROM pg_catalog.pg_depend d WHERE
EXISTS (SELECT 1 FROM scoped s WHERE s.classid = d.classid AND s.objid = d.objid)
OR EXISTS (SELECT 1 FROM scoped s WHERE s.classid = d.refclassid AND s.objid = d.refobjid)`
	var deps []dependency
	err := g.query(query, func(rows *sql.Rows) error {
		for rows.Next() {
			var dep dependency
			if err := rows.Scan(&dep.object.class, &dep.object.oid, &dep.reference.class, &dep.reference.oid, &dep.kind, &dep.local, &dep.refLocal); err != nil {
				return err
			}
			deps = append(deps, dep)
		}
		return nil
	}, g.schemaID)
	return deps, err
}

func orderDrop(objects []dropObject, keys []foreignKey, deps []dependency) ([]dropObject, error) {
	roots := make(map[objectID]bool, len(objects))
	ignored := make(map[objectID]bool, len(keys))
	parents := make(map[objectID][]objectID)
	for _, object := range objects {
		roots[object.id] = true
	}
	for _, key := range keys {
		ignored[key.id] = true
	}
	for _, dep := range deps {
		if dep.kind == "s" {
			continue
		}
		if !dep.local && dep.refLocal || dep.local && (dep.kind == "e" || dep.kind == "x") || dep.local && !dep.refLocal && (dep.kind == "i" || dep.kind == "a") {
			return nil, fmt.Errorf("opengauss: Drop refuses external dependencies or extension-owned objects")
		}
		if dep.local && dep.refLocal && (dep.kind == "a" || dep.kind == "i") {
			parents[dep.object] = append(parents[dep.object], dep.reference)
		}
	}
	for _, dep := range deps {
		if dep.local && !ignored[dep.object] {
			if _, ok := owningRoot(dep.object, roots, parents, make(map[objectID]bool)); !ok {
				return nil, fmt.Errorf("opengauss: Drop refuses unsupported schema objects; remove them explicitly first")
			}
		}
	}
	var edges [][2]objectID
	for _, dep := range deps {
		if dep.kind == "s" || ignored[dep.object] || !dep.local || !dep.refLocal {
			continue
		}
		from, fromOK := owningRoot(dep.object, roots, parents, make(map[objectID]bool))
		to, toOK := owningRoot(dep.reference, roots, parents, make(map[objectID]bool))
		if fromOK && toOK && from != to {
			edges = append(edges, [2]objectID{from, to})
		}
	}
	return topologicalDrop(objects, roots, edges)
}

func owningRoot(id objectID, roots map[objectID]bool, parents map[objectID][]objectID, seen map[objectID]bool) (objectID, bool) {
	if roots[id] {
		return id, true
	}
	if seen[id] {
		return objectID{}, false
	}
	seen[id] = true
	for _, parent := range parents[id] {
		if root, ok := owningRoot(parent, roots, parents, seen); ok {
			return root, true
		}
	}
	return objectID{}, false
}

func topologicalDrop(objects []dropObject, remaining map[objectID]bool, edges [][2]objectID) ([]dropObject, error) {
	var ordered []dropObject
	for len(remaining) > 0 {
		before := len(remaining)
		for _, object := range objects {
			if !remaining[object.id] || hasDependent(object.id, edges, remaining) {
				continue
			}
			ordered = append(ordered, object)
			delete(remaining, object.id)
		}
		if before == len(remaining) {
			return nil, fmt.Errorf("opengauss: cyclic dependencies require explicit migration SQL before Drop")
		}
	}
	return ordered, nil
}

func hasDependent(id objectID, edges [][2]objectID, remaining map[objectID]bool) bool {
	for _, edge := range edges {
		if edge[1] == id && remaining[edge[0]] {
			return true
		}
	}
	return false
}
