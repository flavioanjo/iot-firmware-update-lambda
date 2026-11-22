package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

// A minimal database/sql driver that records statements and returns scripted
// results. It lets the SQL store be tested without a SQL Server instance.

type recordedCall struct {
	query string
	args  []driver.Value
}

type scriptedDriver struct {
	mu       sync.Mutex
	calls    []recordedCall
	rows     [][]driver.Value // returned by every query
	cols     []string
	queryErr error
	affected int64
	open     int // currently open connections
}

func (d *scriptedDriver) Open(string) (driver.Conn, error) {
	d.mu.Lock()
	d.open++
	d.mu.Unlock()
	return &scriptedConn{d: d}, nil
}

func (d *scriptedDriver) record(q string, args []driver.Value) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, recordedCall{q, args})
}

type scriptedConn struct{ d *scriptedDriver }

func (c *scriptedConn) Prepare(q string) (driver.Stmt, error) { return &scriptedStmt{c.d, q}, nil }
func (c *scriptedConn) Close() error {
	c.d.mu.Lock()
	c.d.open--
	c.d.mu.Unlock()
	return nil
}
func (c *scriptedConn) Begin() (driver.Tx, error) { return nil, errors.New("not supported") }

type scriptedStmt struct {
	d *scriptedDriver
	q string
}

func (s *scriptedStmt) Close() error  { return nil }
func (s *scriptedStmt) NumInput() int { return -1 }
func (s *scriptedStmt) Exec(args []driver.Value) (driver.Result, error) {
	s.d.record(s.q, args)
	if s.d.queryErr != nil {
		return nil, s.d.queryErr
	}
	return driver.RowsAffected(s.d.affected), nil
}
func (s *scriptedStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.d.record(s.q, args)
	if s.d.queryErr != nil {
		return nil, s.d.queryErr
	}
	return &scriptedRows{cols: s.d.cols, rows: s.d.rows}, nil
}

type scriptedRows struct {
	cols []string
	rows [][]driver.Value
	i    int
}

func (r *scriptedRows) Columns() []string { return r.cols }
func (r *scriptedRows) Close() error      { return nil }
func (r *scriptedRows) Next(dest []driver.Value) error {
	if r.i >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.i])
	r.i++
	return nil
}

var (
	driverSeq   int
	driverSeqMu sync.Mutex
)

func newScriptedStore(t *testing.T, d *scriptedDriver) sqlStore {
	t.Helper()
	driverSeqMu.Lock()
	driverSeq++
	name := fmt.Sprintf("scripted-%d", driverSeq)
	driverSeqMu.Unlock()
	sql.Register(name, d)
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return sqlStore{db: db}
}

const testMD5 = "0123456789ABCDEF0123456789ABCDEF"

func TestSQLChunkUsesOneBasedSubstring(t *testing.T) {
	d := &scriptedDriver{
		cols: []string{"queue_id", "FirmwareSize", "FirmwareMD5", "Chunk"},
		rows: [][]driver.Value{{int64(55), int64(4096), testMD5, []byte{1, 2, 3}}},
	}
	s := newScriptedStore(t, d)

	c, err := s.Chunk(context.Background(), "dev-1", 2048, 1024)
	if err != nil {
		t.Fatalf("Chunk: %v", err)
	}
	if c.QueueID != int64(55) || c.Size != 4096 || c.MD5Hex != testMD5 || len(c.Data) != 3 {
		t.Fatalf("unexpected chunk %+v", c)
	}
	call := d.calls[0]
	if !strings.Contains(call.query, "SUBSTRING(c.firmware_binary, @p2, @p3)") {
		t.Errorf("query does not use SUBSTRING with parameters: %s", call.query)
	}
	if len(call.args) != 3 || call.args[0] != "dev-1" || call.args[1] != int64(2049) || call.args[2] != int64(1024) {
		t.Errorf("args = %v, want [dev-1 2049 1024]", call.args)
	}
}

func TestSQLQueriesAreParameterized(t *testing.T) {
	for _, q := range []string{metadataQuery, chunkQuery, updateStatusQuery, updateProgressQuery} {
		if strings.Contains(q, "%") || strings.Contains(q, "'+") {
			t.Errorf("query appears to be built dynamically: %s", q)
		}
	}
	for _, q := range []string{metadataQuery, chunkQuery} {
		for _, want := range []string{"device_id = @p1", "command_type IN (221)", "ORDER BY queue_id DESC", "TOP 1"} {
			if !strings.Contains(q, want) {
				t.Errorf("query lost %q: %s", want, q)
			}
		}
	}
}

func TestSQLMetadataMapsErrors(t *testing.T) {
	cols := []string{"queue_id", "FirmwareSize", "FirmwareMD5"}
	cases := []struct {
		name string
		d    *scriptedDriver
		want error
	}{
		{"no rows", &scriptedDriver{cols: cols}, errNoFirmware},
		{"NULL binary", &scriptedDriver{cols: cols, rows: [][]driver.Value{{int64(1), nil, nil}}}, errCorruptFirmware},
		{"driver error", &scriptedDriver{cols: cols, queryErr: errors.New("network down")}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newScriptedStore(t, tc.d).Metadata(context.Background(), "1")
			if err == nil {
				t.Fatal("expected error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if tc.want == nil && (errors.Is(err, errNoFirmware) || errors.Is(err, errCorruptFirmware)) {
				t.Errorf("driver error was misclassified: %v", err)
			}
		})
	}
}

func TestSQLMetadataSuccess(t *testing.T) {
	d := &scriptedDriver{
		cols: []string{"queue_id", "FirmwareSize", "FirmwareMD5"},
		rows: [][]driver.Value{{int64(9), int64(100), testMD5}},
	}
	info, err := newScriptedStore(t, d).Metadata(context.Background(), "abc")
	if err != nil || info.QueueID != int64(9) || info.Size != 100 || info.MD5Hex != testMD5 {
		t.Fatalf("got %+v, %v", info, err)
	}
	if len(d.calls[0].args) != 1 || d.calls[0].args[0] != "abc" {
		t.Errorf("args = %v", d.calls[0].args)
	}
}

func TestSQLUpdatesTargetSelectedQueueRow(t *testing.T) {
	d := &scriptedDriver{affected: 1}
	s := newScriptedStore(t, d)
	if err := s.UpdateStatus(context.Background(), int64(77), statusDownloadStarted); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateProgress(context.Background(), int64(77), 35); err != nil {
		t.Fatal(err)
	}
	if d.calls[0].query != updateStatusQuery || d.calls[0].args[0] != int64(77) || d.calls[0].args[1] != int64(5) {
		t.Errorf("status call = %+v", d.calls[0])
	}
	if d.calls[1].query != updateProgressQuery || d.calls[1].args[0] != int64(77) || d.calls[1].args[1] != int64(35) {
		t.Errorf("progress call = %+v", d.calls[1])
	}
}

func TestSQLUpdateErrors(t *testing.T) {
	d := &scriptedDriver{affected: 0}
	if err := newScriptedStore(t, d).UpdateStatus(context.Background(), int64(1), 6); !errors.Is(err, errJobNotFound) {
		t.Errorf("err = %v, want errJobNotFound", err)
	}
	d = &scriptedDriver{queryErr: errors.New("timeout")}
	if err := newScriptedStore(t, d).UpdateProgress(context.Background(), int64(1), 10); err == nil {
		t.Error("expected error")
	}
}

func TestSQLStoreReleasesConnections(t *testing.T) {
	d := &scriptedDriver{
		cols:     []string{"queue_id", "FirmwareSize", "FirmwareMD5", "Chunk"},
		rows:     [][]driver.Value{{int64(1), int64(10), testMD5, []byte{1}}},
		affected: 1,
	}
	s := newScriptedStore(t, d)
	s.db.SetMaxOpenConns(1)
	// With a single connection, any leaked row set would deadlock here.
	for i := 0; i < 50; i++ {
		if _, err := s.Chunk(context.Background(), "1", 0, 1); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateProgress(context.Background(), int64(1), 5); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.db.Stats().InUse; got != 0 {
		t.Errorf("connections in use = %d, want 0", got)
	}
}
