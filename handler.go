package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"

	"github.com/aws/aws-lambda-go/events"

	_ "github.com/microsoft/go-mssqldb"
)

func HandleRequest(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {

	// Health check for the load balancer and firmware update endpoint
	if request.Path == "/health" || request.Path == "/default/health" || request.Path == "/default/DownloadIOTFirmware" {
		return events.APIGatewayProxyResponse{
			StatusCode: 200,
			Headers: map[string]string{
				"Content-Type": "text/plain",
			},
			Body:            "OK",
			IsBase64Encoded: false,
		}, nil
	}

	deviceID := request.QueryStringParameters["device_id"]

	if deviceID == "" {
		return events.APIGatewayProxyResponse{
			StatusCode: 200,
			Headers: map[string]string{
				"Content-Type": "text/plain",
			},
			Body:            "OK",
			IsBase64Encoded: false,
		}, nil
	}

	offsetStr := request.QueryStringParameters["offset"]
	blockSizeStr := request.QueryStringParameters["block_size"]

	offset := 0
	if offsetStr != "" {
		if val, err := strconv.Atoi(offsetStr); err == nil {
			offset = val
		}
	}

	blockSize := 1024
	if blockSizeStr != "" {
		if val, err := strconv.Atoi(blockSizeStr); err == nil {
			blockSize = val
		}
	}

	// Read the database connection string from the environment for both cloud and local execution
	connString := os.Getenv("DB_CONNECTION_STRING")

	if connString == "" {
		return events.APIGatewayProxyResponse{StatusCode: 500, Body: "Internal error: database connection string is not configured."}, nil
	}

	// Open the database connection pool
	db, err := sql.Open("sqlserver", connString)
	if err != nil {
		return events.APIGatewayProxyResponse{StatusCode: 500, Body: "Error opening the database connection."}, nil
	}
	defer db.Close()

	// ============================================================
	// OFFSET -1: Return firmware metadata (4-byte size + 16-byte MD5)
	// ============================================================
	if offset == -1 {
		metadataQuery := `
            SELECT DATALENGTH(firmware_binary) as FirmwareSize,
                   CONVERT(VARCHAR(32), HASHBYTES('MD5', firmware_binary), 2) as FirmwareMD5
            FROM dbo.firmware_catalog
            WHERE firmware_id=(
                SELECT TOP 1 firmware_id
                FROM dbo.device_firmware_queue
                WHERE (device_id=@p1) AND (command_type IN (221))
                ORDER BY queue_id DESC
        )`

		var totalSize int
		var firmwareMD5Hex string

		err = db.QueryRowContext(ctx, metadataQuery, deviceID).Scan(&totalSize, &firmwareMD5Hex)
		if err != nil {
			if err == sql.ErrNoRows {
				return events.APIGatewayProxyResponse{
					StatusCode: 404,
					Body:       fmt.Sprintf("No pending firmware for device %s", deviceID),
				}, nil
			}
			return events.APIGatewayProxyResponse{
				StatusCode: 500,
				Body:       fmt.Sprintf("Database query error: %v", err),
			}, nil
		}

		// Update status to 5 to mark the firmware download as started
		updateStatus := `
            UPDATE dbo.device_firmware_queue SET status = 5
            WHERE queue_id = (
                SELECT TOP 1 queue_id
                FROM dbo.device_firmware_queue
                WHERE (device_id=@p1) AND (command_type IN (221))
                ORDER BY queue_id DESC
            )`
		_, _ = db.ExecContext(ctx, updateStatus, deviceID)

		// Build the metadata packet: 4 bytes (little-endian int32) + 16 bytes (raw MD5)
		metadataPacket := make([]byte, 20)
		binary.LittleEndian.PutUint32(metadataPacket[0:4], uint32(totalSize))

		md5Bytes, _ := hex.DecodeString(firmwareMD5Hex)
		copy(metadataPacket[4:20], md5Bytes)

		encodedMetadata := base64.StdEncoding.EncodeToString(metadataPacket)

		return events.APIGatewayProxyResponse{
			StatusCode: 200,
			Headers: map[string]string{
				"Content-Type":   "application/octet-stream",
				"Content-Length": "20",
			},
			Body:            encodedMetadata,
			IsBase64Encoded: true,
		}, nil
	}

	// ============================================================
	// OFFSET >= 0: Return the firmware data chunk
	// ============================================================

	query := `
            SELECT DATALENGTH(firmware_binary) as FirmwareSize,
                   CONVERT(VARCHAR(32), HASHBYTES('MD5', firmware_binary), 2) as FirmwareMD5,
                   SUBSTRING(firmware_binary, @p2, @p3) as Chunk
            FROM dbo.firmware_catalog
            WHERE firmware_id=(
                SELECT TOP 1 firmware_id
                FROM dbo.device_firmware_queue
                WHERE (device_id=@p1) AND (command_type IN (221))
                ORDER BY queue_id DESC
        )`

	// SQL Server SUBSTRING starts at 1, not 0
	offsetSQL := offset + 1

	var totalSize int
	var firmwareMD5 string
	var chunkData []byte

	err = db.QueryRowContext(ctx, query, deviceID, offsetSQL, blockSize).Scan(&totalSize, &firmwareMD5, &chunkData)

	if err != nil {
		if err == sql.ErrNoRows {
			return events.APIGatewayProxyResponse{
				StatusCode: 404,
				Body:       fmt.Sprintf("No pending firmware for device %s", deviceID),
			}, nil
		}
		return events.APIGatewayProxyResponse{
			StatusCode: 500,
			Body:       fmt.Sprintf("Database query error: %v", err),
		}, nil
	}

	if offset >= totalSize {
		// Update status to 6 to mark the firmware download as complete
		updateStatus := `
            UPDATE dbo.device_firmware_queue SET status = 6
            WHERE queue_id = (
                SELECT TOP 1 queue_id
                FROM dbo.device_firmware_queue
                WHERE (device_id=@p1) AND (command_type IN (221))
                ORDER BY queue_id DESC
            )`
		_, _ = db.ExecContext(ctx, updateStatus, deviceID)

		return events.APIGatewayProxyResponse{
			StatusCode: 200,
			Headers: map[string]string{
				"Content-Type": "text/plain",
			},
			Body:            fmt.Sprintf("*** MD5 = %s ***", firmwareMD5),
			IsBase64Encoded: false,
		}, nil
	}

	chunkSize := len(chunkData)

	// Calculate the current and previous progress to persist updates every 5%
	currentProgress := ((offset + blockSize) * 100) / totalSize
	if currentProgress > 100 {
		currentProgress = 100
	}
	previousProgress := (offset * 100) / totalSize

	// Persist progress only when crossing a 5% threshold
	if (currentProgress / 5) > (previousProgress / 5) {
		updateProgress := `
            UPDATE dbo.device_firmware_queue SET progress = @p2
            WHERE queue_id = (
                SELECT TOP 1 queue_id
                FROM dbo.device_firmware_queue
                WHERE (device_id=@p1) AND (command_type IN (221))
                ORDER BY queue_id DESC
            )`
		_, _ = db.ExecContext(ctx, updateProgress, deviceID, currentProgress)
	}

	base64Content := base64.StdEncoding.EncodeToString(chunkData)

	return events.APIGatewayProxyResponse{
		StatusCode: 200,
		Headers: map[string]string{
			"Content-Type":        "application/octet-stream",
			"Content-Disposition": fmt.Sprintf("attachment; filename=\"firmware_%s_part.bin\"", deviceID),
			"Content-Length":      fmt.Sprintf("%d", chunkSize),
			"X-Firmware-MD5":      firmwareMD5,
			"X-Firmware-Size":     fmt.Sprintf("%d", totalSize),
		},
		Body:            base64Content,
		IsBase64Encoded: true,
	}, nil
}
