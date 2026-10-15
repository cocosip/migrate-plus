package gaussdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gaussdbgo "github.com/HuaweiCloudDeveloper/gaussdb-go"
	"github.com/HuaweiCloudDeveloper/gaussdb-go/gaussdbconn"
	"github.com/HuaweiCloudDeveloper/gaussdb-go/gaussdbproto"
	"github.com/HuaweiCloudDeveloper/gaussdb-go/stdlib"
	"github.com/golang-migrate/migrate/v4/database"
)

// A real native adapter over an in-memory wire verifies behavior sqlmock cannot model.
func protocolDriver(t *testing.T, respond func(*gaussdbproto.Backend, string) byte) (*GaussDB, <-chan string) {
	t.Helper()
	client, server := net.Pipe()
	queries := make(chan string, 16)
	done := make(chan error, 1)
	go func() {
		defer func() { _ = server.Close() }()
		backend := gaussdbproto.NewBackend(server, server)
		if _, err := backend.ReceiveStartupMessage(); err != nil {
			done <- err
			return
		}
		backend.Send(&gaussdbproto.AuthenticationOk{})
		backend.Send(&gaussdbproto.ParameterStatus{Name: "standard_conforming_strings", Value: "on"})
		backend.Send(&gaussdbproto.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
		backend.Send(&gaussdbproto.ReadyForQuery{TxStatus: 'I'})
		if err := backend.Flush(); err != nil {
			done <- err
			return
		}
		for {
			message, err := backend.Receive()
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				done <- err
				return
			}
			switch message := message.(type) {
			case *gaussdbproto.Terminate:
				done <- nil
				return
			case *gaussdbproto.Query:
				if message.String == scriptSettingsSQL {
					backend.Send(&gaussdbproto.RowDescription{Fields: []gaussdbproto.FieldDescription{
						{Name: []byte("mode"), DataTypeOID: 25, DataTypeSize: -1},
						{Name: []byte("standard"), DataTypeOID: 25, DataTypeSize: -1},
						{Name: []byte("encoding"), DataTypeOID: 25, DataTypeSize: -1},
					}})
					backend.Send(&gaussdbproto.DataRow{Values: [][]byte{[]byte("PG"), []byte("on"), []byte("UTF8")}})
					backend.Send(&gaussdbproto.CommandComplete{CommandTag: []byte("SELECT 1")})
					backend.Send(&gaussdbproto.ReadyForQuery{TxStatus: 'I'})
					if err := backend.Flush(); err != nil {
						done <- err
						return
					}
					continue
				}
				queries <- message.String
				status := respond(backend, message.String)
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
	config, err := gaussdbgo.ParseConfig("gaussdb://test@localhost/app?sslmode=disable&default_query_exec_mode=simple_protocol")
	if err != nil {
		t.Fatal(err)
	}
	var connected atomic.Bool
	config.DialFunc = func(context.Context, string, string) (net.Conn, error) {
		if !connected.CompareAndSwap(false, true) {
			return nil, fmt.Errorf("unexpected replacement connection")
		}
		return client, nil
	}
	db := stdlib.OpenDB(*config)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = server.Close()
		_ = client.Close()
		t.Fatal(err)
	}
	lockCtx, stop := context.WithCancel(context.Background())
	g := &GaussDB{db: db, conn: conn, config: Config{StatementTimeout: time.Second},
		caps: capabilities{mode: "PG", standardStrings: true}, sessionStatus: nativeStatus,
		lockCtx: lockCtx, stopLock: stop}
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
	return g, queries
}

func TestNativeAdapterExecutesWholeFileAndRejectsCOPY(t *testing.T) {
	g, queries := protocolDriver(t, func(backend *gaussdbproto.Backend, _ string) byte {
		backend.Send(&gaussdbproto.CommandComplete{CommandTag: []byte("SELECT 1")})
		backend.Send(&gaussdbproto.CommandComplete{CommandTag: []byte("SELECT 1")})
		return 'I'
	})
	script := "SELECT 1; SELECT 2;"
	if err := g.Run(strings.NewReader(script)); err != nil {
		t.Fatal(err)
	}
	if actual := <-queries; actual != script {
		t.Fatalf("wire SQL changed: %q", actual)
	}
	if err := g.Run(strings.NewReader("COPY t FROM STDIN;")); err == nil {
		t.Fatal("COPY reached native adapter")
	}
	select {
	case query := <-queries:
		t.Fatalf("COPY sent to server: %q", query)
	default:
	}
}

func TestNativeAdapterRollsBackOpenAndFailedTransactions(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprint(failed), func(t *testing.T) {
			g, queries := protocolDriver(t, func(backend *gaussdbproto.Backend, query string) byte {
				if query == rollbackSQL {
					backend.Send(&gaussdbproto.CommandComplete{CommandTag: []byte(rollbackSQL)})
					return 'I'
				}
				if failed {
					backend.Send(&gaussdbproto.ErrorResponse{Severity: "ERROR", Code: "42601", Message: "test syntax failure"})
					return 'E'
				}
				backend.Send(&gaussdbproto.CommandComplete{CommandTag: []byte("BEGIN")})
				return 'T'
			})
			err := g.Run(strings.NewReader("BEGIN; SELECT 1;"))
			if err == nil || !strings.Contains(err.Error(), "transaction") {
				t.Fatalf("unfinished transaction = %v", err)
			}
			if failed {
				var dbErr *database.Error
				var native *gaussdbconn.GaussdbError
				if !errors.As(err, &dbErr) || !errors.As(dbErr.OrigErr, &native) || native.SQLState() != "42601" {
					t.Fatalf("lost native SQLSTATE: %v", err)
				}
			}
			if <-queries != "BEGIN; SELECT 1;" || <-queries != rollbackSQL {
				t.Fatal("transaction cleanup did not reach original session")
			}
			if status, err := nativeStatus(g.conn); err != nil || status != 'I' {
				t.Fatalf("native status after cleanup = %q, %v", status, err)
			}
		})
	}
}
