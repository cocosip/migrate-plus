package acceptance

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *suite) crossSchema() {
	if s.target.OtherTarget == "" {
		s.record("cross-schema", Skipped, fmt.Errorf("other_target with cross-schema SELECT/REFERENCES privileges is required"), 0)
		return
	}
	if s.unsafe {
		s.record("cross-schema", Blocked, fmt.Errorf("prior cleanup/preflight failed"), 0)
		return
	}
	var peer Target
	for _, t := range s.all {
		if t.Name == s.target.OtherTarget {
			peer = t
		}
	}
	start := time.Now()
	err := s.crossSchemaRun(peer)
	s.record("cross-schema", statusFor(err), err, time.Since(start))
}

func (s *suite) crossSchemaRun(peer Target) (result error) {
	if peer.Name == "" {
		return fmt.Errorf("paired target is missing")
	}
	dsn, err := peer.resolveDSN()
	if err != nil {
		return err
	}
	other, err := connect(s.ctx, peer, dsn)
	if err != nil {
		return fmt.Errorf("peer: %s", redactConnection(err.Error(), dsn))
	}
	defer func() { result = errors.Join(result, other.close()) }()
	if err := s.c.peerCompatible(other); err != nil {
		return err
	}
	peerSuite := &suite{ctx: s.ctx, c: other, profile: s.profile, target: peer}
	if err := s.requireEmpty(); err != nil {
		return err
	}
	if err := peerSuite.requireEmpty(); err != nil {
		return err
	}
	defer func() {
		// Remove the owned dependent before either schema's cleanup; preserve all errors.
		ctx, cancel := context.WithTimeout(context.Background(), peer.statementTimeout())
		defer cancel()
		if err := other.checkIdentity(ctx); err != nil {
			result = errors.Join(result, err)
			return
		}
		objects, err := other.inventory(ctx)
		if err == nil {
			err = checkOwned(objects)
		}
		if err != nil {
			result = errors.Join(result, err)
			return
		}
		query := "DROP VIEW " + other.qualified("mp_external_view")
		if exists, err := containsObject(ctx, other, "mp_external_view"); err != nil {
			result = errors.Join(result, err)
		} else if exists {
			_, err = other.conn.ExecContext(ctx, query)
			result = errors.Join(result, err)
		}
		result = errors.Join(result, peerSuite.cleanup(), s.cleanup())
	}()
	if err := s.exec("CREATE TABLE mp_external(id INTEGER PRIMARY KEY); INSERT INTO mp_external VALUES(7);"); err != nil {
		return err
	}
	if err := peerSuite.exec("CREATE TABLE mp_sentinel(id INTEGER PRIMARY KEY); INSERT INTO mp_sentinel VALUES(9); CREATE VIEW mp_external_view AS SELECT id FROM " + s.c.qualified("mp_external")); err != nil {
		return err
	}
	if err := s.d.Drop(); err == nil {
		return fmt.Errorf("drop accepted external dependent")
	} else if !strings.Contains(strings.ToLower(err.Error()), "dependenc") {
		return fmt.Errorf("wrong external-dependency error: %w", err)
	}
	if err := s.want("SELECT id FROM mp_external", "7"); err != nil {
		return err
	}
	if err := peerSuite.want("SELECT id FROM mp_sentinel", "9"); err != nil {
		return err
	}
	if err := peerSuite.want("SELECT id FROM mp_external_view", "7"); err != nil {
		return err
	}
	if err := peerSuite.exec("DROP VIEW mp_external_view"); err != nil {
		return err
	}
	if err := peerSuite.exec("CREATE TABLE mp_external_child(id INTEGER, CONSTRAINT mp_external_fk FOREIGN KEY(id) REFERENCES " + s.c.qualified("mp_external") + "(id)); INSERT INTO mp_external_child VALUES(7);"); err != nil {
		return err
	}
	if err := s.d.Drop(); err == nil {
		return fmt.Errorf("drop accepted external foreign key")
	} else if !strings.Contains(strings.ToLower(err.Error()), "dependenc") {
		return fmt.Errorf("wrong foreign-key dependency error: %w", err)
	}
	if err := s.want("SELECT id FROM mp_external", "7"); err != nil {
		return err
	}
	if err := peerSuite.want("SELECT id FROM mp_external_child", "7"); err != nil {
		return err
	}
	if err := peerSuite.exec(peerSuite.dropTable("mp_external_child")); err != nil {
		return err
	}
	if err := s.d.Drop(); err != nil {
		return err
	}
	if err := s.closeMigrator(); err != nil {
		return err
	}
	if err := s.requireEmpty(); err != nil {
		return err
	}
	return peerSuite.want("SELECT id FROM mp_sentinel", "9")
}

func containsObject(ctx context.Context, c *connection, name string) (bool, error) {
	objects, err := c.inventory(ctx)
	for _, o := range objects {
		if strings.EqualFold(o.Name, name) {
			return true, err
		}
	}
	return false, err
}
