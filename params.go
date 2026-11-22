package main

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/aws/aws-lambda-go/events"
)

// requestParams are the validated query parameters of a firmware request.
type requestParams struct {
	deviceID  string
	offset    int64
	blockSize int
}

// errHealthProbe signals a request without device_id and without download
// parameters. The original implementation answered such requests with
// "200 OK", and the device simulator uses that as its health check.
var errHealthProbe = errors.New("health probe")

// paramError is a client error whose message is safe to return verbatim.
type paramError string

func (e paramError) Error() string { return string(e) }

// parseParams validates the query string.
//
// Compatibility rules preserved from the original implementation:
//   - an absent or empty offset means 0;
//   - an absent or empty block_size means 1024;
//   - block_size is ignored (not validated) for metadata requests (offset=-1);
//   - parameters other than device_id, offset and block_size are ignored.
func parseParams(request events.APIGatewayProxyRequest, maxBlockSize int) (requestParams, error) {
	deviceID, err := singleParam(request, "device_id")
	if err != nil {
		return requestParams{}, err
	}
	offsetStr, err := singleParam(request, "offset")
	if err != nil {
		return requestParams{}, err
	}
	blockSizeStr, err := singleParam(request, "block_size")
	if err != nil {
		return requestParams{}, err
	}

	if deviceID == "" {
		if offsetStr == "" && blockSizeStr == "" {
			return requestParams{}, errHealthProbe
		}
		return requestParams{}, paramError("missing device_id")
	}
	if err := validateDeviceID(deviceID); err != nil {
		return requestParams{}, err
	}

	p := requestParams{deviceID: deviceID, blockSize: defaultBlockSize}

	if offsetStr != "" {
		offset, err := strconv.ParseInt(offsetStr, 10, 64)
		if err != nil {
			return requestParams{}, paramError("invalid offset: must be an integer")
		}
		if offset < metadataOffset {
			return requestParams{}, paramError("invalid offset: must be -1 (metadata) or a non-negative byte offset")
		}
		if offset > maxOffset {
			return requestParams{}, paramError(fmt.Sprintf("invalid offset: must not exceed %d", int64(maxOffset)))
		}
		p.offset = offset
	}

	if p.offset == metadataOffset {
		return p, nil
	}

	if blockSizeStr != "" {
		blockSize, err := strconv.ParseInt(blockSizeStr, 10, 64)
		if err != nil {
			return requestParams{}, paramError("invalid block_size: must be an integer")
		}
		if blockSize <= 0 {
			return requestParams{}, paramError("invalid block_size: must be greater than zero")
		}
		if blockSize > int64(maxBlockSize) {
			return requestParams{}, paramError(fmt.Sprintf("invalid block_size: must not exceed %d", maxBlockSize))
		}
		p.blockSize = int(blockSize)
	}
	return p, nil
}

// singleParam returns a query parameter and rejects conflicting duplicates
// (for example offset=0&offset=1024), which API Gateway would otherwise
// silently resolve to the last value.
func singleParam(request events.APIGatewayProxyRequest, name string) (string, error) {
	if values := request.MultiValueQueryStringParameters[name]; len(values) > 1 {
		for _, v := range values[1:] {
			if v != values[0] {
				return "", paramError(fmt.Sprintf("conflicting values for %s", name))
			}
		}
		return values[0], nil
	}
	return request.QueryStringParameters[name], nil
}

// validateDeviceID accepts 1-64 characters from [A-Za-z0-9._:-]. The value is
// always sent to SQL Server as a parameter; the character set additionally
// keeps it safe to echo in the Content-Disposition header and error bodies.
func validateDeviceID(id string) error {
	if len(id) > maxDeviceIDLength {
		return paramError(fmt.Sprintf("invalid device_id: must not exceed %d characters", maxDeviceIDLength))
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == ':':
		default:
			return paramError("invalid device_id: allowed characters are A-Z, a-z, 0-9, '-', '_', '.' and ':'")
		}
	}
	return nil
}
