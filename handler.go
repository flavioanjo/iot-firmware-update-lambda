package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"sync"

	"github.com/aws/aws-lambda-go/events"
)

// Wire protocol constants.
const (
	// metadataOffset is the control value that requests the 20-byte metadata
	// packet instead of a firmware chunk.
	metadataOffset = -1
	// metadataPacketSize is 4 bytes (little-endian uint32 size) + 16 bytes MD5.
	metadataPacketSize = 20
	// defaultBlockSize is used when block_size is absent or empty.
	defaultBlockSize = 1024
	// defaultMaxBlockSize is the default upper bound for block_size (512 KiB).
	// Base64 inflates the body by 4/3, so a full block (~683 KiB encoded)
	// stays below the 1 MB ALB Lambda response limit and far below the 6 MB
	// synchronous Lambda payload limit. Configurable through MAX_BLOCK_SIZE.
	defaultMaxBlockSize = 512 * 1024
	// hardMaxBlockSize is the largest value MAX_BLOCK_SIZE may be set to (4 MiB
	// raw, ~5.3 MiB base64), the most a synchronous Lambda response can carry.
	hardMaxBlockSize = 4 * 1024 * 1024
	// maxOffset is the largest offset accepted. The metadata packet encodes the
	// firmware size as a uint32, so no valid offset can be larger.
	maxOffset = math.MaxUint32
	// maxDeviceIDLength bounds the device_id parameter.
	maxDeviceIDLength = 64
	// progressStep is the granularity, in percent, of persisted progress.
	progressStep = 5
)

// healthPaths short-circuit to a plain "OK" response before any parameter is
// read. "/default/DownloadIOTFirmware" is kept for backward compatibility with
// existing load balancer health checks; see README "Known limitations".
var healthPaths = map[string]bool{
	"/health":                      true,
	"/default/health":              true,
	"/default/DownloadIOTFirmware": true,
}

// firmwareHandler implements the firmware download protocol.
type firmwareHandler struct {
	store        func() (firmwareStore, error)
	queryTimeout timeoutFunc
	maxBlockSize int
	logger       *slog.Logger
}

// timeoutFunc derives the per-request database context.
type timeoutFunc func(context.Context) (context.Context, context.CancelFunc)

func newFirmwareHandler(cfg config, store func() (firmwareStore, error), logger *slog.Logger) *firmwareHandler {
	return &firmwareHandler{
		store: store,
		queryTimeout: func(ctx context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(ctx, cfg.QueryTimeout)
		},
		maxBlockSize: cfg.MaxBlockSize,
		logger:       logger,
	}
}

var (
	defaultHandlerOnce sync.Once
	defaultHandler     *firmwareHandler
)

// newLogger returns the structured JSON logger used by the service.
func newLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, nil))
}

// HandleRequest is the AWS Lambda entry point. It keeps the original exported
// signature and delegates to a process-wide handler whose SQL Server
// connection pool is reused across warm invocations.
func HandleRequest(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	defaultHandlerOnce.Do(func() {
		logger := newLogger()
		cfg := loadConfig(logger)
		pool := &sharedPool{cfg: cfg}
		defaultHandler = newFirmwareHandler(cfg, pool.store, logger)
	})
	return defaultHandler.Handle(ctx, request)
}

// Handle serves health checks, metadata requests and chunk requests. It never
// returns a non-nil error: every failure is mapped to an HTTP response so the
// caller always receives a well-formed reply.
func (h *firmwareHandler) Handle(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	if healthPaths[request.Path] {
		return textResponse(http.StatusOK, "OK"), nil
	}

	params, err := parseParams(request, h.maxBlockSize)
	if errors.Is(err, errHealthProbe) {
		// A request without device_id is answered as a health check, matching
		// the original behavior relied on by the device simulator and probes.
		return textResponse(http.StatusOK, "OK"), nil
	}
	if err != nil {
		return textResponse(http.StatusBadRequest, err.Error()), nil
	}

	store, err := h.store()
	if err != nil {
		h.logger.Error("database unavailable", slog.String("reason", err.Error()))
		return textResponse(http.StatusInternalServerError, "Internal error: database is not configured."), nil
	}

	dbCtx, cancel := h.queryTimeout(ctx)
	defer cancel()

	log := h.logger.With(slog.String("device_id", params.deviceID), slog.Int64("offset", params.offset))
	if params.offset == metadataOffset {
		return h.handleMetadata(dbCtx, log, store, params), nil
	}
	return h.handleChunk(dbCtx, log, store, params), nil
}

func (h *firmwareHandler) handleMetadata(ctx context.Context, log *slog.Logger, store firmwareStore, p requestParams) events.APIGatewayProxyResponse {
	info, err := store.Metadata(ctx, p.deviceID)
	if err != nil {
		return h.lookupFailure(ctx, log, p, err)
	}
	md5Bytes, err := validateFirmware(info)
	if err != nil {
		return h.internalError(ctx, log, "invalid firmware metadata", err)
	}

	if err := store.UpdateStatus(ctx, info.QueueID, statusDownloadStarted); err != nil {
		if !errors.Is(err, errJobNotFound) {
			return h.internalError(ctx, log, "status update failed", err)
		}
		log.Warn("status update matched no job", slog.Int("status", statusDownloadStarted))
	}

	return events.APIGatewayProxyResponse{
		StatusCode: http.StatusOK,
		Headers: map[string]string{
			"Content-Type":   "application/octet-stream",
			"Content-Length": strconv.Itoa(metadataPacketSize),
		},
		Body:            base64.StdEncoding.EncodeToString(metadataPacket(uint32(info.Size), md5Bytes)),
		IsBase64Encoded: true,
	}
}

func (h *firmwareHandler) handleChunk(ctx context.Context, log *slog.Logger, store firmwareStore, p requestParams) events.APIGatewayProxyResponse {
	chunk, err := store.Chunk(ctx, p.deviceID, p.offset, p.blockSize)
	if err != nil {
		return h.lookupFailure(ctx, log, p, err)
	}
	if _, err := validateFirmware(chunk.firmwareInfo); err != nil {
		return h.internalError(ctx, log, "invalid firmware metadata", err)
	}

	if p.offset >= chunk.Size {
		// End of file. Compatibility note: the status becomes "download
		// complete" for any offset >= size, even if the device skipped chunks.
		// It does not mean the firmware was installed.
		if err := store.UpdateStatus(ctx, chunk.QueueID, statusDownloadComplete); err != nil {
			if !errors.Is(err, errJobNotFound) {
				return h.internalError(ctx, log, "status update failed", err)
			}
			log.Warn("status update matched no job", slog.Int("status", statusDownloadComplete))
		}
		return textResponse(http.StatusOK, fmt.Sprintf("*** MD5 = %s ***", chunk.MD5Hex))
	}

	expected := min(int64(p.blockSize), chunk.Size-p.offset)
	if int64(len(chunk.Data)) != expected {
		return h.internalError(ctx, log, "unexpected chunk length",
			fmt.Errorf("%w: got %d bytes, expected %d", errCorruptFirmware, len(chunk.Data), expected))
	}

	if progress, ok := progressUpdate(p.offset, int64(p.blockSize), chunk.Size); ok {
		// Progress is advisory: a failed write is logged but does not fail the
		// transfer, so a transient write error never interrupts a download.
		if err := store.UpdateProgress(ctx, chunk.QueueID, progress); err != nil {
			log.Warn("progress update failed", slog.Int("progress", progress), slog.String("error", err.Error()))
		}
	}

	return events.APIGatewayProxyResponse{
		StatusCode: http.StatusOK,
		Headers: map[string]string{
			"Content-Type":        "application/octet-stream",
			"Content-Disposition": fmt.Sprintf("attachment; filename=\"firmware_%s_part.bin\"", p.deviceID),
			"Content-Length":      strconv.Itoa(len(chunk.Data)),
			"X-Firmware-MD5":      chunk.MD5Hex,
			"X-Firmware-Size":     strconv.FormatInt(chunk.Size, 10),
		},
		Body:            base64.StdEncoding.EncodeToString(chunk.Data),
		IsBase64Encoded: true,
	}
}

// lookupFailure maps an error from the firmware lookup to a response.
func (h *firmwareHandler) lookupFailure(ctx context.Context, log *slog.Logger, p requestParams, err error) events.APIGatewayProxyResponse {
	if errors.Is(err, errNoFirmware) {
		return textResponse(http.StatusNotFound, fmt.Sprintf("No pending firmware for device %s", p.deviceID))
	}
	if errors.Is(err, errCorruptFirmware) {
		return h.internalError(ctx, log, "invalid firmware record", err)
	}
	return h.internalError(ctx, log, "firmware lookup failed", err)
}

// internalError logs the detailed cause and returns a generic message, so SQL
// errors and server details never reach API consumers. Timeouts and
// cancellations become 503 to signal that a retry may succeed.
func (h *firmwareHandler) internalError(ctx context.Context, log *slog.Logger, msg string, err error) events.APIGatewayProxyResponse {
	log.Error(msg, slog.String("error", err.Error()))
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || ctx.Err() != nil {
		resp := textResponse(http.StatusServiceUnavailable, "Service temporarily unavailable, please retry.")
		resp.Headers["Retry-After"] = "1"
		return resp
	}
	return textResponse(http.StatusInternalServerError, "Internal server error.")
}

// validateFirmware checks the values that end up on the wire and returns the
// raw MD5 bytes.
func validateFirmware(info firmwareInfo) ([]byte, error) {
	if info.Size < 0 || info.Size > math.MaxUint32 {
		return nil, fmt.Errorf("%w: size %d does not fit the 4-byte size field", errCorruptFirmware, info.Size)
	}
	md5Bytes, err := hex.DecodeString(info.MD5Hex)
	if err != nil || len(md5Bytes) != 16 {
		return nil, fmt.Errorf("%w: checksum is not a 32-character hex MD5", errCorruptFirmware)
	}
	return md5Bytes, nil
}

// metadataPacket builds the 20-byte metadata payload.
func metadataPacket(size uint32, md5Bytes []byte) []byte {
	packet := make([]byte, metadataPacketSize)
	binary.LittleEndian.PutUint32(packet[0:4], size)
	copy(packet[4:20], md5Bytes)
	return packet
}

// progressUpdate reproduces the original progress rule: progress is computed
// from the requested window end (offset+blockSize), capped at 100, and is only
// persisted when the request crosses a progressStep boundary. total must be
// greater than offset (and therefore greater than zero).
func progressUpdate(offset, blockSize, total int64) (int, bool) {
	current := min((offset+blockSize)*100/total, 100)
	previous := offset * 100 / total
	if current/progressStep > previous/progressStep {
		return int(current), true
	}
	return 0, false
}

func textResponse(status int, body string) events.APIGatewayProxyResponse {
	return events.APIGatewayProxyResponse{
		StatusCode:      status,
		Headers:         map[string]string{"Content-Type": "text/plain"},
		Body:            body,
		IsBase64Encoded: false,
	}
}
