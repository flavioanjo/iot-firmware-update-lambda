package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
)

// ---------------------------------------------------------------------------
// Test doubles
// ---------------------------------------------------------------------------

type fakeJob struct {
	queueID  int64
	firmware []byte
	md5Hex   string // overrides the computed MD5 when non-empty
	size     int64  // overrides len(firmware) when non-zero
}

type statusCall struct {
	queueID any
	status  int
}

type progressCall struct {
	queueID  any
	progress int
}

// fakeStore emulates the SQL store in memory: offset is zero-based and the
// chunk follows SQL Server SUBSTRING semantics (truncated at the end, empty
// beyond it).
type fakeStore struct {
	mu          sync.Mutex
	jobs        map[string]fakeJob
	lookupErr   error
	statusErr   error
	progressErr error
	statuses    []statusCall
	progresses  []progressCall
	chunkCalls  int
}

func newFakeStore() *fakeStore { return &fakeStore{jobs: map[string]fakeJob{}} }

func (s *fakeStore) add(deviceID string, job fakeJob) { s.jobs[deviceID] = job }

func (s *fakeStore) info(deviceID string) (fakeJob, firmwareInfo, error) {
	if s.lookupErr != nil {
		return fakeJob{}, firmwareInfo{}, s.lookupErr
	}
	job, ok := s.jobs[deviceID]
	if !ok {
		return fakeJob{}, firmwareInfo{}, errNoFirmware
	}
	sum := md5.Sum(job.firmware)
	info := firmwareInfo{QueueID: job.queueID, Size: int64(len(job.firmware)), MD5Hex: strings.ToUpper(hex.EncodeToString(sum[:]))}
	if job.md5Hex != "" {
		info.MD5Hex = job.md5Hex
	}
	if job.size != 0 {
		info.Size = job.size
	}
	return job, info, nil
}

func (s *fakeStore) Metadata(ctx context.Context, deviceID string) (firmwareInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, info, err := s.info(deviceID)
	return info, err
}

func (s *fakeStore) Chunk(ctx context.Context, deviceID string, offset int64, length int) (firmwareChunk, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chunkCalls++
	job, info, err := s.info(deviceID)
	if err != nil {
		return firmwareChunk{}, err
	}
	var data []byte
	if offset < int64(len(job.firmware)) {
		end := min(offset+int64(length), int64(len(job.firmware)))
		data = append([]byte(nil), job.firmware[offset:end]...)
	}
	return firmwareChunk{firmwareInfo: info, Data: data}, nil
}

func (s *fakeStore) UpdateStatus(ctx context.Context, queueID any, status int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statusErr != nil {
		return s.statusErr
	}
	s.statuses = append(s.statuses, statusCall{queueID, status})
	return nil
}

func (s *fakeStore) UpdateProgress(ctx context.Context, queueID any, progress int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.progressErr != nil {
		return s.progressErr
	}
	s.progresses = append(s.progresses, progressCall{queueID, progress})
	return nil
}

func testConfig() config {
	return config{QueryTimeout: 5 * time.Second, MaxBlockSize: defaultMaxBlockSize}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestHandler(store firmwareStore) *firmwareHandler {
	return newFirmwareHandler(testConfig(), func() (firmwareStore, error) { return store, nil }, discardLogger())
}

func query(kv ...string) events.APIGatewayProxyRequest {
	req := events.APIGatewayProxyRequest{QueryStringParameters: map[string]string{}}
	for i := 0; i+1 < len(kv); i += 2 {
		req.QueryStringParameters[kv[i]] = kv[i+1]
	}
	return req
}

func mustDecode(t *testing.T, resp events.APIGatewayProxyResponse) []byte {
	t.Helper()
	if !resp.IsBase64Encoded {
		t.Fatalf("expected base64 body, got plain %q", resp.Body)
	}
	b, err := base64.StdEncoding.DecodeString(resp.Body)
	if err != nil {
		t.Fatalf("invalid base64 body: %v", err)
	}
	return b
}

func firmwareOf(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + 3)
	}
	return b
}

func md5Upper(b []byte) string {
	sum := md5.Sum(b)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

func call(t *testing.T, h *firmwareHandler, req events.APIGatewayProxyRequest) events.APIGatewayProxyResponse {
	t.Helper()
	resp, err := h.Handle(context.Background(), req)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	return resp
}

// ---------------------------------------------------------------------------
// Metadata
// ---------------------------------------------------------------------------

func TestMetadataRequest(t *testing.T) {
	fw := firmwareOf(3000)
	store := newFakeStore()
	store.add("12345", fakeJob{queueID: 42, firmware: fw})
	h := newTestHandler(store)

	resp := call(t, h, query("device_id", "12345", "offset", "-1"))

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body %q", resp.StatusCode, resp.Body)
	}
	packet := mustDecode(t, resp)
	if len(packet) != 20 {
		t.Fatalf("metadata length = %d, want 20", len(packet))
	}
	if got := binary.LittleEndian.Uint32(packet[0:4]); got != 3000 {
		t.Errorf("size = %d, want 3000", got)
	}
	sum := md5.Sum(fw)
	if !bytes.Equal(packet[4:20], sum[:]) {
		t.Errorf("md5 = %X, want %X", packet[4:20], sum)
	}
	if resp.Headers["Content-Type"] != "application/octet-stream" || resp.Headers["Content-Length"] != "20" {
		t.Errorf("unexpected headers: %v", resp.Headers)
	}
	if len(store.statuses) != 1 || store.statuses[0] != (statusCall{int64(42), statusDownloadStarted}) {
		t.Errorf("status updates = %v, want one update to %d for queue 42", store.statuses, statusDownloadStarted)
	}
	if store.chunkCalls != 0 {
		t.Errorf("metadata request should not read chunks")
	}
}

func TestMetadataIgnoresBlockSize(t *testing.T) {
	// The original implementation never looked at block_size for offset=-1;
	// a device that sends e.g. block_size=0 there must keep working.
	store := newFakeStore()
	store.add("1", fakeJob{queueID: 1, firmware: firmwareOf(10)})
	h := newTestHandler(store)
	for _, bs := range []string{"0", "-5", "abc", "99999999999"} {
		resp := call(t, h, query("device_id", "1", "offset", "-1", "block_size", bs))
		if resp.StatusCode != 200 {
			t.Errorf("block_size=%q: status %d (%s)", bs, resp.StatusCode, resp.Body)
		}
	}
}

func TestCorruptedMetadata(t *testing.T) {
	cases := []struct {
		name string
		job  fakeJob
		err  error
	}{
		{"non-hex checksum", fakeJob{queueID: 1, firmware: firmwareOf(10), md5Hex: strings.Repeat("Z", 32)}, nil},
		{"short checksum", fakeJob{queueID: 1, firmware: firmwareOf(10), md5Hex: "ABCD"}, nil},
		{"size beyond uint32", fakeJob{queueID: 1, firmware: firmwareOf(10), size: 1 << 33}, nil},
		{"negative size", fakeJob{queueID: 1, firmware: firmwareOf(10), size: -1}, nil},
		{"NULL columns", fakeJob{}, fmt.Errorf("%w: NULL", errCorruptFirmware)},
	}
	for _, tc := range cases {
		for _, offset := range []string{"-1", "0"} {
			t.Run(tc.name+"/offset="+offset, func(t *testing.T) {
				store := newFakeStore()
				store.add("1", tc.job)
				store.lookupErr = tc.err
				resp := call(t, newTestHandler(store), query("device_id", "1", "offset", offset))
				if resp.StatusCode != 500 {
					t.Fatalf("status = %d, want 500", resp.StatusCode)
				}
				if len(store.statuses) != 0 {
					t.Errorf("status must not change for corrupt firmware: %v", store.statuses)
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// Chunks and EOF
// ---------------------------------------------------------------------------

func TestChunkRequest(t *testing.T) {
	fw := firmwareOf(2500)
	cases := []struct {
		name      string
		offset    string
		blockSize string
		want      []byte
	}{
		{"first chunk default block size", "0", "", fw[0:1024]},
		{"missing offset defaults to 0", "", "", fw[0:1024]},
		{"middle chunk", "1024", "1024", fw[1024:2048]},
		{"last partial chunk", "2048", "1024", fw[2048:2500]},
		{"single byte", "2499", "1", fw[2499:2500]},
		{"block larger than firmware", "0", "4096", fw},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			store.add("dev-1", fakeJob{queueID: 7, firmware: fw})
			kv := []string{"device_id", "dev-1"}
			if tc.offset != "" {
				kv = append(kv, "offset", tc.offset)
			}
			if tc.blockSize != "" {
				kv = append(kv, "block_size", tc.blockSize)
			}
			resp := call(t, newTestHandler(store), query(kv...))
			if resp.StatusCode != 200 {
				t.Fatalf("status = %d, body %q", resp.StatusCode, resp.Body)
			}
			got := mustDecode(t, resp)
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("chunk mismatch: got %d bytes, want %d", len(got), len(tc.want))
			}
			wantHeaders := map[string]string{
				"Content-Type":        "application/octet-stream",
				"Content-Disposition": `attachment; filename="firmware_dev-1_part.bin"`,
				"Content-Length":      strconv.Itoa(len(tc.want)),
				"X-Firmware-MD5":      md5Upper(fw),
				"X-Firmware-Size":     "2500",
			}
			for k, v := range wantHeaders {
				if resp.Headers[k] != v {
					t.Errorf("header %s = %q, want %q", k, resp.Headers[k], v)
				}
			}
			if len(store.statuses) != 0 {
				t.Errorf("chunk requests must not change status: %v", store.statuses)
			}
		})
	}
}

func TestEndOfFile(t *testing.T) {
	fw := firmwareOf(100)
	for _, offset := range []string{"100", "101", "4096", strconv.FormatInt(maxOffset, 10)} {
		t.Run("offset="+offset, func(t *testing.T) {
			store := newFakeStore()
			store.add("1", fakeJob{queueID: 9, firmware: fw})
			resp := call(t, newTestHandler(store), query("device_id", "1", "offset", offset, "block_size", "10"))
			if resp.StatusCode != 200 || resp.IsBase64Encoded {
				t.Fatalf("status = %d base64 = %v", resp.StatusCode, resp.IsBase64Encoded)
			}
			if want := "*** MD5 = " + md5Upper(fw) + " ***"; resp.Body != want {
				t.Errorf("body = %q, want %q", resp.Body, want)
			}
			if resp.Headers["Content-Type"] != "text/plain" {
				t.Errorf("content type = %q", resp.Headers["Content-Type"])
			}
			if len(store.statuses) != 1 || store.statuses[0] != (statusCall{int64(9), statusDownloadComplete}) {
				t.Errorf("status updates = %v, want one update to %d", store.statuses, statusDownloadComplete)
			}
			if len(store.progresses) != 0 {
				t.Errorf("EOF must not write progress: %v", store.progresses)
			}
		})
	}
}

func TestEmptyFirmwareDoesNotDivideByZero(t *testing.T) {
	store := newFakeStore()
	store.add("1", fakeJob{queueID: 1, firmware: []byte{}})
	resp := call(t, newTestHandler(store), query("device_id", "1", "offset", "0"))
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Body, "*** MD5 = ") {
		t.Fatalf("unexpected response %d %q", resp.StatusCode, resp.Body)
	}
}

func TestUnexpectedChunkLengthIsRejected(t *testing.T) {
	store := &shortChunkStore{fakeStore: newFakeStore()}
	store.add("1", fakeJob{queueID: 1, firmware: firmwareOf(100)})
	resp := call(t, newTestHandler(store), query("device_id", "1", "offset", "0", "block_size", "10"))
	if resp.StatusCode != 500 {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

type shortChunkStore struct{ *fakeStore }

func (s *shortChunkStore) Chunk(ctx context.Context, id string, off int64, n int) (firmwareChunk, error) {
	c, err := s.fakeStore.Chunk(ctx, id, off, n)
	c.Data = c.Data[:len(c.Data)-1]
	return c, err
}

// ---------------------------------------------------------------------------
// Missing firmware and validation
// ---------------------------------------------------------------------------

func TestMissingFirmware(t *testing.T) {
	for _, offset := range []string{"-1", "0", "2048"} {
		store := newFakeStore()
		resp := call(t, newTestHandler(store), query("device_id", "777", "offset", offset))
		if resp.StatusCode != 404 {
			t.Errorf("offset %s: status = %d, want 404", offset, resp.StatusCode)
		}
		if resp.Body != "No pending firmware for device 777" {
			t.Errorf("offset %s: body = %q", offset, resp.Body)
		}
		if len(store.statuses) != 0 {
			t.Errorf("offset %s: unexpected status update", offset)
		}
	}
}

func TestInvalidRequests(t *testing.T) {
	cases := []struct {
		name string
		req  events.APIGatewayProxyRequest
		want string
	}{
		{"offset not a number", query("device_id", "1", "offset", "abc"), "invalid offset"},
		{"offset decimal", query("device_id", "1", "offset", "1.5"), "invalid offset"},
		{"offset hex", query("device_id", "1", "offset", "0x10"), "invalid offset"},
		{"offset -2", query("device_id", "1", "offset", "-2"), "invalid offset"},
		{"offset very negative", query("device_id", "1", "offset", "-9223372036854775808"), "invalid offset"},
		{"offset overflows int64", query("device_id", "1", "offset", "99999999999999999999"), "invalid offset"},
		{"offset beyond uint32", query("device_id", "1", "offset", "4294967296"), "invalid offset"},
		{"block_size zero", query("device_id", "1", "offset", "0", "block_size", "0"), "invalid block_size"},
		{"block_size negative", query("device_id", "1", "offset", "0", "block_size", "-1024"), "invalid block_size"},
		{"block_size not a number", query("device_id", "1", "offset", "0", "block_size", "1k"), "invalid block_size"},
		{"block_size oversized", query("device_id", "1", "offset", "0", "block_size", strconv.Itoa(defaultMaxBlockSize+1)), "invalid block_size"},
		{"block_size overflows", query("device_id", "1", "offset", "0", "block_size", "99999999999999999999"), "invalid block_size"},
		{"device_id missing with offset", query("offset", "0"), "missing device_id"},
		{"device_id missing with block_size", query("block_size", "512"), "missing device_id"},
		{"device_id with space", query("device_id", "12 34", "offset", "0"), "invalid device_id"},
		{"device_id header injection", query("device_id", "1\r\nX-Evil: 1", "offset", "0"), "invalid device_id"},
		{"device_id SQL", query("device_id", "1' OR '1'='1", "offset", "0"), "invalid device_id"},
		{"device_id path", query("device_id", "../x", "offset", "0"), "invalid device_id"},
		{"device_id too long", query("device_id", strings.Repeat("9", 65), "offset", "0"), "invalid device_id"},
		{"conflicting duplicate offset", events.APIGatewayProxyRequest{
			QueryStringParameters:           map[string]string{"device_id": "1", "offset": "1024"},
			MultiValueQueryStringParameters: map[string][]string{"device_id": {"1"}, "offset": {"0", "1024"}},
		}, "conflicting values for offset"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			store.add("1", fakeJob{queueID: 1, firmware: firmwareOf(10)})
			resp := call(t, newTestHandler(store), tc.req)
			if resp.StatusCode != 400 {
				t.Fatalf("status = %d, want 400 (body %q)", resp.StatusCode, resp.Body)
			}
			if !strings.Contains(resp.Body, tc.want) {
				t.Errorf("body = %q, want it to contain %q", resp.Body, tc.want)
			}
			if store.chunkCalls != 0 || len(store.statuses) != 0 {
				t.Errorf("invalid requests must not reach the database")
			}
		})
	}
}

func TestValidEdgeParameters(t *testing.T) {
	store := newFakeStore()
	id := strings.Repeat("A", maxDeviceIDLength)
	store.add(id, fakeJob{queueID: 1, firmware: firmwareOf(10)})
	store.add("imei:3520-99_01.x", fakeJob{queueID: 2, firmware: firmwareOf(10)})
	h := newTestHandler(store)
	for _, req := range []events.APIGatewayProxyRequest{
		query("device_id", id, "offset", "0"),
		query("device_id", "imei:3520-99_01.x", "offset", "0"),
		query("device_id", id, "offset", "0", "block_size", strconv.Itoa(defaultMaxBlockSize)),
		{
			QueryStringParameters:           map[string]string{"device_id": id, "offset": "0"},
			MultiValueQueryStringParameters: map[string][]string{"device_id": {id, id}},
		},
		query("device_id", id, "offset", "0", "cache_buster", "123"),
	} {
		if resp := call(t, h, req); resp.StatusCode != 200 {
			t.Errorf("%v: status %d (%s)", req.QueryStringParameters, resp.StatusCode, resp.Body)
		}
	}
}

func TestHealthChecks(t *testing.T) {
	failing := func() (firmwareStore, error) { return nil, errors.New("must not be called") }
	h := newFirmwareHandler(testConfig(), failing, discardLogger())
	cases := []events.APIGatewayProxyRequest{
		{Path: "/health"},
		{Path: "/default/health"},
		{Path: "/default/DownloadIOTFirmware"},
		{Path: "/default/DownloadIOTFirmware", QueryStringParameters: map[string]string{"device_id": "1", "offset": "0"}},
		{},
		query("device_id", ""),
		query("unrelated", "x"),
	}
	for _, req := range cases {
		resp := call(t, h, req)
		if resp.StatusCode != 200 || resp.Body != "OK" {
			t.Errorf("path %q params %v: got %d %q", req.Path, req.QueryStringParameters, resp.StatusCode, resp.Body)
		}
	}
}

// ---------------------------------------------------------------------------
// Database failures
// ---------------------------------------------------------------------------

func TestDatabaseNotConfigured(t *testing.T) {
	pool := &sharedPool{cfg: config{}}
	h := newFirmwareHandler(testConfig(), pool.store, discardLogger())
	resp := call(t, h, query("device_id", "1", "offset", "0"))
	if resp.StatusCode != 500 {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
}

func TestDatabaseFailureDoesNotLeakDetails(t *testing.T) {
	secret := "mssql: Login failed for user 'svc_firmware' on server db-internal-01"
	for _, offset := range []string{"-1", "0"} {
		store := newFakeStore()
		store.lookupErr = errors.New(secret)
		resp := call(t, newTestHandler(store), query("device_id", "1", "offset", offset))
		if resp.StatusCode != 500 {
			t.Fatalf("status = %d, want 500", resp.StatusCode)
		}
		if strings.Contains(resp.Body, "mssql") || strings.Contains(resp.Body, "svc_firmware") {
			t.Errorf("response leaks database error: %q", resp.Body)
		}
	}
}

func TestDatabaseTimeout(t *testing.T) {
	store := newFakeStore()
	store.lookupErr = fmt.Errorf("read: %w", context.DeadlineExceeded)
	resp := call(t, newTestHandler(store), query("device_id", "1", "offset", "0"))
	if resp.StatusCode != 503 || resp.Headers["Retry-After"] == "" {
		t.Fatalf("status = %d headers %v, want 503 with Retry-After", resp.StatusCode, resp.Headers)
	}
}

func TestQueryTimeoutIsApplied(t *testing.T) {
	store := &blockingStore{}
	cfg := testConfig()
	cfg.QueryTimeout = 20 * time.Millisecond
	h := newFirmwareHandler(cfg, func() (firmwareStore, error) { return store, nil }, discardLogger())
	start := time.Now()
	resp := call(t, h, query("device_id", "1", "offset", "-1"))
	if resp.StatusCode != 503 {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("query timeout not honoured")
	}
}

type blockingStore struct{ fakeStore }

func (*blockingStore) Metadata(ctx context.Context, _ string) (firmwareInfo, error) {
	<-ctx.Done()
	return firmwareInfo{}, ctx.Err()
}

func TestStatusUpdateFailure(t *testing.T) {
	for _, offset := range []string{"-1", "100"} {
		store := newFakeStore()
		store.add("1", fakeJob{queueID: 1, firmware: firmwareOf(100)})
		store.statusErr = errors.New("deadlock victim")
		resp := call(t, newTestHandler(store), query("device_id", "1", "offset", offset))
		if resp.StatusCode != 500 {
			t.Errorf("offset %s: status = %d, want 500 so the device retries", offset, resp.StatusCode)
		}
	}
}

func TestStatusUpdateJobVanished(t *testing.T) {
	store := newFakeStore()
	store.add("1", fakeJob{queueID: 1, firmware: firmwareOf(100)})
	store.statusErr = errJobNotFound
	resp := call(t, newTestHandler(store), query("device_id", "1", "offset", "-1"))
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestProgressUpdateFailureDoesNotFailChunk(t *testing.T) {
	store := newFakeStore()
	store.add("1", fakeJob{queueID: 1, firmware: firmwareOf(100)})
	store.progressErr = errors.New("write failed")
	resp := call(t, newTestHandler(store), query("device_id", "1", "offset", "0", "block_size", "50"))
	if resp.StatusCode != 200 || len(mustDecode(t, resp)) != 50 {
		t.Fatalf("status = %d, want 200 with chunk", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Progress
// ---------------------------------------------------------------------------

func TestProgressUpdate(t *testing.T) {
	cases := []struct {
		offset, block, total int64
		want                 int
		write                bool
	}{
		{0, 1024, 10240, 10, true},                              // 0% -> 10%
		{0, 10, 1000, 1, false},                                 // 0% -> 1%: below first step
		{40, 10, 1000, 5, true},                                 // 4% -> 5%
		{50, 10, 1000, 6, false},                                // 5% -> 6%: same step
		{990, 10, 1000, 100, true},                              // 99% -> 100%
		{900, 1024, 1000, 100, true},                            // capped at 100
		{0, 1, 1, 100, true},                                    // single-byte firmware
		{maxOffset - 1, hardMaxBlockSize, maxOffset, 100, true}, // no overflow at the limits
	}
	for _, tc := range cases {
		got, write := progressUpdate(tc.offset, tc.block, tc.total)
		if write != tc.write || (write && got != tc.want) {
			t.Errorf("progressUpdate(%d,%d,%d) = (%d,%v), want (%d,%v)",
				tc.offset, tc.block, tc.total, got, write, tc.want, tc.write)
		}
	}
}

func TestProgressPersistedDuringDownload(t *testing.T) {
	store := newFakeStore()
	store.add("1", fakeJob{queueID: 3, firmware: firmwareOf(1000)})
	h := newTestHandler(store)
	for off := 0; off < 1000; off += 100 {
		call(t, h, query("device_id", "1", "offset", strconv.Itoa(off), "block_size", "100"))
	}
	var got []int
	for _, p := range store.progresses {
		if p.queueID != int64(3) {
			t.Errorf("progress written to queue %v", p.queueID)
		}
		got = append(got, p.progress)
	}
	if want := []int{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("progress = %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// End-to-end protocol, resume and concurrency
// ---------------------------------------------------------------------------

// download replays the device simulator algorithm and returns the image.
func download(t *testing.T, h *firmwareHandler, deviceID string, block, startOffset int, prefix []byte) []byte {
	t.Helper()
	meta := call(t, h, query("device_id", deviceID, "offset", "-1"))
	if meta.StatusCode != 200 {
		t.Errorf("metadata status %d", meta.StatusCode)
		return nil
	}
	packet := mustDecode(t, meta)
	size := int(binary.LittleEndian.Uint32(packet[0:4]))
	out := append([]byte(nil), prefix...)
	for offset := startOffset; ; offset += block {
		resp := call(t, h, query("device_id", deviceID, "offset", strconv.Itoa(offset), "block_size", strconv.Itoa(block)))
		if resp.StatusCode != 200 {
			t.Errorf("chunk status %d", resp.StatusCode)
			return nil
		}
		if !resp.IsBase64Encoded {
			if want := "*** MD5 = " + strings.ToUpper(hex.EncodeToString(packet[4:20])) + " ***"; resp.Body != want {
				t.Errorf("EOF body %q, want %q", resp.Body, want)
			}
			break
		}
		out = append(out, mustDecode(t, resp)...)
		if offset > size+block {
			t.Errorf("download did not terminate")
			return nil
		}
	}
	if len(out) != size {
		t.Errorf("downloaded %d bytes, metadata said %d", len(out), size)
	}
	return out
}

func TestFullDownloadMatchesMD5(t *testing.T) {
	for _, size := range []int{1, 1023, 1024, 1025, 10_000} {
		for _, block := range []int{1, 512, 1024, 4096} {
			if size/block > 2000 {
				continue
			}
			fw := firmwareOf(size)
			store := newFakeStore()
			store.add("1", fakeJob{queueID: 1, firmware: fw})
			got := download(t, newTestHandler(store), "1", block, 0, nil)
			if !bytes.Equal(got, fw) {
				t.Errorf("size %d block %d: image mismatch", size, block)
			}
		}
	}
}

func TestResumedDownload(t *testing.T) {
	fw := firmwareOf(5000)
	store := newFakeStore()
	store.add("1", fakeJob{queueID: 1, firmware: fw})
	h := newTestHandler(store)
	// First attempt stops after two chunks; the device resumes at 2048 and
	// repeats a chunk it already had. Requests are idempotent.
	first := call(t, h, query("device_id", "1", "offset", "1024", "block_size", "1024"))
	again := call(t, h, query("device_id", "1", "offset", "1024", "block_size", "1024"))
	if first.Body != again.Body {
		t.Fatalf("repeated request returned different data")
	}
	got := download(t, h, "1", 1024, 2048, fw[:2048])
	if !bytes.Equal(got, fw) {
		t.Fatalf("resumed image mismatch")
	}
}

func TestConcurrentDownloads(t *testing.T) {
	store := newFakeStore()
	images := map[string][]byte{}
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("dev-%d", i)
		images[id] = firmwareOf(3000 + i*517)
		store.add(id, fakeJob{queueID: int64(i), firmware: images[id]})
	}
	h := newTestHandler(store)

	var wg sync.WaitGroup
	for id, fw := range images {
		for rep := 0; rep < 3; rep++ { // several concurrent transfers per device
			wg.Add(1)
			go func() {
				defer wg.Done()
				if got := download(t, h, id, 256, 0, nil); !bytes.Equal(got, fw) {
					t.Errorf("%s: image mismatch", id)
				}
			}()
		}
	}
	wg.Wait()
}

func TestSharedPoolIsReused(t *testing.T) {
	pool := &sharedPool{cfg: config{ConnString: "sqlserver://user:pass@127.0.0.1:1?database=x", MaxOpenConns: 2, MaxIdleConns: 5}}
	a, err := pool.store()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	b, _ := pool.store()
	if a.(sqlStore).db != b.(sqlStore).db {
		t.Fatal("pool was not reused")
	}
	if got := a.(sqlStore).db.Stats().MaxOpenConnections; got != 2 {
		t.Errorf("MaxOpenConnections = %d, want 2", got)
	}
	_ = a.(sqlStore).db.Close()
}

func TestHandleRequestEntryPoint(t *testing.T) {
	// The exported entry point must keep working for health checks without
	// any database configuration.
	resp, err := HandleRequest(context.Background(), events.APIGatewayProxyRequest{Path: "/health"})
	if err != nil || resp.StatusCode != 200 || resp.Body != "OK" {
		t.Fatalf("got %d %q %v", resp.StatusCode, resp.Body, err)
	}
}
