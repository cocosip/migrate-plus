package opengauss

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	opq "gitcode.com/opengauss/openGauss-connector-go-pq"
	"github.com/HuaweiCloudDeveloper/gaussdb-go/gaussdbproto"
)

func TestNativeTransactionCleanup(t *testing.T) {
	for _, tc := range []struct {
		status byte
		script string
	}{
		{'I', committedScript},
		{'T', "BEGIN; SELECT 1;"},
		{'E', "BEGIN; SELECT * FROM missing;"},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			g, rollbacks := nativeRunDriver(t, tc.status)
			wantRollbacks := int32(0)
			if tc.status != 'I' {
				wantRollbacks = 1
			}
			err := g.Run(strings.NewReader(tc.script))
			if (err != nil) != (tc.status != 'I') || g.broken.Load() || rollbacks.Load() != wantRollbacks {
				t.Fatalf("Run = %v, broken=%v, rollbacks=%d", err, g.broken.Load(), rollbacks.Load())
			}
			if err := g.Run(strings.NewReader("SELECT 2;")); err != nil || rollbacks.Load() != wantRollbacks {
				t.Fatalf("Run after cleanup = %v, rollbacks=%d", err, rollbacks.Load())
			}
		})
	}
}

func nativeRunDriver(t *testing.T, scriptStatus byte) (*OpenGauss, *atomic.Int32) {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan error, 1)
	rollbacks := new(atomic.Int32)
	go func() {
		defer func() { _ = server.Close() }()
		// The client sends protocol 3.51; only the startup packet framing is needed here.
		var length uint32
		if err := binary.Read(server, binary.BigEndian, &length); err != nil {
			done <- err
			return
		}
		if _, err := io.CopyN(io.Discard, server, int64(length)-4); err != nil {
			done <- err
			return
		}
		backend := gaussdbproto.NewBackend(server, server)
		backend.Send(&gaussdbproto.AuthenticationOk{})
		backend.Send(&gaussdbproto.ReadyForQuery{TxStatus: 'I'})
		if err := backend.Flush(); err != nil {
			done <- err
			return
		}
		status := byte('I')
		for {
			message, err := backend.Receive()
			if err != nil {
				done <- err
				return
			}
			switch message := message.(type) {
			case *gaussdbproto.Terminate:
				done <- nil
				return
			case *gaussdbproto.Query:
				switch message.String {
				case scriptSettingsSQL:
					backend.Send(&gaussdbproto.RowDescription{Fields: []gaussdbproto.FieldDescription{
						{Name: []byte("mode"), DataTypeOID: 25, DataTypeSize: -1},
						{Name: []byte("standard"), DataTypeOID: 25, DataTypeSize: -1},
						{Name: []byte("encoding"), DataTypeOID: 25, DataTypeSize: -1},
					}})
					backend.Send(&gaussdbproto.DataRow{Values: [][]byte{[]byte("PG"), []byte("on"), []byte("UTF8")}})
					backend.Send(&gaussdbproto.CommandComplete{CommandTag: []byte("SELECT 1")})
				case transactionProbeSQL:
					if status == 'I' {
						backend.Send(&gaussdbproto.ErrorResponse{Severity: nativeErrorSeverity, Code: "25P01", Message: "no active transaction"})
					} else {
						backend.Send(&gaussdbproto.CommandComplete{CommandTag: []byte("SAVEPOINT")})
					}
				case rollbackSQL:
					rollbacks.Add(1)
					status = 'I'
					backend.Send(&gaussdbproto.CommandComplete{CommandTag: []byte(rollbackSQL)})
				default:
					status = scriptStatus
					if message.String == "SELECT 2;" {
						status = 'I'
					}
					if status == 'E' {
						backend.Send(&gaussdbproto.ErrorResponse{Severity: nativeErrorSeverity, Code: "42601", Message: "test SQL failure"})
					} else {
						backend.Send(&gaussdbproto.CommandComplete{CommandTag: []byte("SELECT 1")})
					}
				}
				backend.Send(&gaussdbproto.ReadyForQuery{TxStatus: status})
				if err := backend.Flush(); err != nil {
					done <- err
					return
				}
			default:
				done <- fmt.Errorf("unexpected native message %T", message)
				return
			}
		}
	}()
	config, err := opq.ParseConfig("opengauss://test@localhost/app?sslmode=disable&target_session_attrs=any")
	if err != nil {
		t.Fatal(err)
	}
	config.DialFunc = func(context.Context, string, string) (net.Conn, error) { return client, nil }
	connector, err := opq.NewConnectorConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = client.Close()
		_ = server.Close()
		_ = db.Close()
		t.Fatal(err)
	}
	lockCtx, stop := context.WithCancel(context.Background())
	g := &OpenGauss{db: db, conn: conn, config: Config{StatementTimeout: time.Second}, lockCtx: lockCtx, stopLock: stop}
	t.Cleanup(func() {
		if err := g.Close(); err != nil {
			t.Error(err)
		}
		_ = server.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("native backend did not finish")
		}
	})
	return g, rollbacks
}
