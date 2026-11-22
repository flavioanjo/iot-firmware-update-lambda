package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
)

// Identifiers of the existing database protocol. They are part of the
// contract with the systems that enqueue firmware jobs and read their status,
// so they must not be changed without coordinating with those systems.
const (
	// commandTypeFirmwareUpdate is the command_type of firmware update jobs in
	// dbo.device_firmware_queue.
	commandTypeFirmwareUpdate = 221
	// statusDownloadStarted is written when a device requests the metadata.
	statusDownloadStarted = 5
	// statusDownloadComplete is written when a device requests an offset at
	// or beyond the end of the firmware image.
	statusDownloadComplete = 6
)

var (
	// errNoFirmware means the device has no firmware update job, or the job
	// references a firmware image that does not exist in the catalog.
	errNoFirmware = errors.New("no pending firmware for device")
	// errCorruptFirmware means the firmware record exists but cannot be served
	// (for example a NULL binary or an unexpected checksum).
	errCorruptFirmware = errors.New("firmware record is invalid")
	// errJobNotFound means an UPDATE matched no queue row, typically because
	// the job was removed while the request was in progress.
	errJobNotFound = errors.New("firmware job no longer exists")
)

// firmwareInfo describes the firmware selected for a device.
type firmwareInfo struct {
	// QueueID identifies the dbo.device_firmware_queue row that selected the
	// firmware. It is kept as the driver-native value so the service makes no
	// assumption about the column type; it is only passed back to UPDATEs.
	QueueID any
	// Size is DATALENGTH(firmware_binary) in bytes.
	Size int64
	// MD5Hex is the uppercase hexadecimal MD5 computed by SQL Server.
	MD5Hex string
}

// firmwareChunk is a firmware slice plus the metadata of the whole image.
type firmwareChunk struct {
	firmwareInfo
	Data []byte
}

// firmwareStore is the persistence boundary used by the handler. It exists so
// the request handling logic can be tested without a SQL Server instance.
type firmwareStore interface {
	// Metadata returns the size and MD5 of the firmware selected for deviceID.
	Metadata(ctx context.Context, deviceID string) (firmwareInfo, error)
	// Chunk returns up to length bytes starting at the zero-based offset.
	Chunk(ctx context.Context, deviceID string, offset int64, length int) (firmwareChunk, error)
	// UpdateStatus sets the status column of the given queue row.
	UpdateStatus(ctx context.Context, queueID any, status int) error
	// UpdateProgress sets the progress column (0-100) of the given queue row.
	UpdateProgress(ctx context.Context, queueID any, progress int) error
}

// latestJobQuery selects the most recent firmware update job of a device and
// joins the referenced firmware image. The derived table keeps the original
// semantics: only the latest job is considered, and if its firmware is missing
// from the catalog the result is empty (404) instead of falling back to an
// older job. %s receives optional extra columns.
const latestJobQuery = `
SELECT q.queue_id,
       DATALENGTH(c.firmware_binary) AS FirmwareSize,
       CONVERT(VARCHAR(32), HASHBYTES('MD5', c.firmware_binary), 2) AS FirmwareMD5%s
FROM (
    SELECT TOP 1 queue_id, firmware_id
    FROM dbo.device_firmware_queue
    WHERE device_id = @p1 AND command_type IN (221)
    ORDER BY queue_id DESC
) AS q
INNER JOIN dbo.firmware_catalog AS c ON c.firmware_id = q.firmware_id`

var (
	metadataQuery = fmt.Sprintf(latestJobQuery, "")
	// SQL Server SUBSTRING is 1-based; @p2 receives offset+1.
	chunkQuery = fmt.Sprintf(latestJobQuery, `,
       SUBSTRING(c.firmware_binary, @p2, @p3) AS Chunk`)
)

const (
	updateStatusQuery   = `UPDATE dbo.device_firmware_queue SET status = @p2 WHERE queue_id = @p1`
	updateProgressQuery = `UPDATE dbo.device_firmware_queue SET progress = @p2 WHERE queue_id = @p1`
)

// sqlStore implements firmwareStore on top of a shared *sql.DB pool.
type sqlStore struct {
	db *sql.DB
}

func (s sqlStore) Metadata(ctx context.Context, deviceID string) (firmwareInfo, error) {
	var (
		queueID any
		size    sql.NullInt64
		md5Hex  sql.NullString
	)
	err := s.db.QueryRowContext(ctx, metadataQuery, deviceID).Scan(&queueID, &size, &md5Hex)
	if err != nil {
		return firmwareInfo{}, mapQueryError(err)
	}
	if !size.Valid || !md5Hex.Valid {
		return firmwareInfo{}, fmt.Errorf("%w: NULL size or checksum", errCorruptFirmware)
	}
	return firmwareInfo{QueueID: queueID, Size: size.Int64, MD5Hex: md5Hex.String}, nil
}

func (s sqlStore) Chunk(ctx context.Context, deviceID string, offset int64, length int) (firmwareChunk, error) {
	var (
		queueID any
		size    sql.NullInt64
		md5Hex  sql.NullString
		data    []byte
	)
	start := offset + 1 // SQL Server SUBSTRING starts at 1, not 0.
	err := s.db.QueryRowContext(ctx, chunkQuery, deviceID, start, int64(length)).
		Scan(&queueID, &size, &md5Hex, &data)
	if err != nil {
		return firmwareChunk{}, mapQueryError(err)
	}
	if !size.Valid || !md5Hex.Valid {
		return firmwareChunk{}, fmt.Errorf("%w: NULL size or checksum", errCorruptFirmware)
	}
	return firmwareChunk{
		firmwareInfo: firmwareInfo{QueueID: queueID, Size: size.Int64, MD5Hex: md5Hex.String},
		Data:         data,
	}, nil
}

func (s sqlStore) UpdateStatus(ctx context.Context, queueID any, status int) error {
	return s.exec(ctx, updateStatusQuery, queueID, int64(status))
}

func (s sqlStore) UpdateProgress(ctx context.Context, queueID any, progress int) error {
	return s.exec(ctx, updateProgressQuery, queueID, int64(progress))
}

func (s sqlStore) exec(ctx context.Context, query string, queueID any, value int64) error {
	res, err := s.db.ExecContext(ctx, query, queueID, value)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errJobNotFound
	}
	return nil
}

func mapQueryError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return errNoFirmware
	}
	return err
}

// errDatabaseNotConfigured is returned when DB_CONNECTION_STRING is empty.
var errDatabaseNotConfigured = errors.New("DB_CONNECTION_STRING is not configured")

// sharedPool lazily opens a single *sql.DB per process. In AWS Lambda the
// pool survives between invocations of a warm execution environment, which
// avoids a TCP + TLS + login handshake on every request. Initialization is
// retried on the next request if it fails.
type sharedPool struct {
	mu  sync.Mutex
	db  *sql.DB
	cfg config
}

func (p *sharedPool) store() (firmwareStore, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.db != nil {
		return sqlStore{db: p.db}, nil
	}
	if p.cfg.ConnString == "" {
		return nil, errDatabaseNotConfigured
	}
	// sql.Open only validates the connection string; it does not connect.
	db, err := sql.Open("sqlserver", p.cfg.ConnString)
	if err != nil {
		// The driver error may echo parts of the connection string, so it is
		// intentionally not wrapped or logged.
		return nil, errors.New("invalid DB_CONNECTION_STRING")
	}
	db.SetMaxOpenConns(p.cfg.MaxOpenConns)
	db.SetMaxIdleConns(min(p.cfg.MaxIdleConns, p.cfg.MaxOpenConns))
	db.SetConnMaxLifetime(p.cfg.ConnMaxLifetime)
	db.SetConnMaxIdleTime(p.cfg.ConnMaxIdleTime)
	p.db = db
	return sqlStore{db: db}, nil
}
