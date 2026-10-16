package gaussdb

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
)

func TestInitializationLockUsesUpstreamTimeout(t *testing.T) {
	previous := migrate.DefaultLockTimeout
	migrate.DefaultLockTimeout = 5 * time.Millisecond
	t.Cleanup(func() { migrate.DefaultLockTimeout = previous })
	db, mock := mockDB(t)
	expectMetadata(mock, "A", 0)
	expectLock(mock, false)
	mock.ExpectClose()
	if _, err := withInstance(db, nil, idleStatus); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("initialization did not honor lock timeout: %v", err)
	}
}
