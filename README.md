# IoT Firmware Update Lambda

A small Go service that delivers firmware images to IoT devices over HTTP in
fixed-size chunks. It runs on AWS Lambda (custom runtime) behind API Gateway or
an Application Load Balancer, or as a local HTTP server during development.
Firmware images and update jobs live in SQL Server.

> **Status:** reference implementation / portfolio project. It has automated
> tests against in-memory doubles but has not been load-tested or audited for a
> specific production environment. Read [Security assumptions](#security-assumptions)
> and [Known limitations](#known-limitations) before deploying it.

## Contents

- [Architecture](#architecture)
- [Download protocol](#download-protocol)
- [API reference](#api-reference)
- [Configuration](#configuration)
- [Running locally](#running-locally)
- [Testing](#testing)
- [Building and deploying](#building-and-deploying)
- [Security assumptions](#security-assumptions)
- [Known limitations](#known-limitations)
- [Project layout](#project-layout)

## Architecture

```
┌────────┐  HTTPS GET   ┌────────────────────────┐  invoke  ┌──────────────────┐  T-SQL   ┌────────────┐
│ Device │ ───────────► │ API Gateway / ALB      │ ───────► │ Lambda (Go,      │ ───────► │ SQL Server │
│        │ ◄─────────── │ (auth, TLS, throttling)│ ◄─────── │ provided.al2023) │ ◄─────── │            │
└────────┘  binary body └────────────────────────┘  base64  └──────────────────┘          └────────────┘
```

| File          | Responsibility                                                     |
|---------------|--------------------------------------------------------------------|
| `main.go`     | Entry point: `lambda.Start` or the local HTTP server.              |
| `handler.go`  | Protocol logic: health checks, metadata, chunks, EOF, progress.    |
| `params.go`   | Query string validation.                                           |
| `store.go`    | SQL Server queries and the shared connection pool.                 |
| `config.go`   | Environment variables and defaults.                                |
| `local.go`    | HTTP ⇄ API Gateway event adapter used in local mode.               |
| `teste/`      | Stand-alone device simulator (separate Go module, stdlib only).    |

### AWS Lambda execution model

- The handler receives an `events.APIGatewayProxyRequest` (API Gateway REST /
  HTTP API payload v1.0; ALB events have a compatible shape).
- Binary bodies are returned base64-encoded with `IsBase64Encoded: true`; API
  Gateway (with binary media types configured) or the ALB decodes them.
- One `*sql.DB` pool is created lazily per execution environment and reused by
  every warm invocation, so a cold start pays the SQL Server login once instead
  of once per request. Each request's queries are bounded by `DB_QUERY_TIMEOUT`
  and by the Lambda deadline, whichever is earlier.
- The handler never returns a Go error to the runtime: every failure is mapped to
  an HTTP status, so clients always receive a well-formed response.

### Database contract

The service reads and updates two existing tables. It does **not** create or
migrate them.

| Table                         | Columns used                                                     |
|-------------------------------|------------------------------------------------------------------|
| `dbo.device_firmware_queue`   | `queue_id`, `device_id`, `firmware_id`, `command_type`, `status`, `progress` |
| `dbo.firmware_catalog`        | `firmware_id`, `firmware_binary`                                 |

- A device's job is the row with the highest `queue_id` where
  `device_id = @device` and `command_type = 221` (firmware update). The job's
  `status` is **not** used for selection (see [Known limitations](#known-limitations)).
- `status` values written by this service:

  | Value | Meaning            | Written when                                   |
  |-------|--------------------|------------------------------------------------|
  | `5`   | Download started   | the device requests metadata (`offset=-1`)     |
  | `6`   | Download complete  | the device requests an `offset` ≥ firmware size |

  `6` means "all bytes were offered", not "firmware installed".
- `progress` (0–100) is written whenever a chunk request crosses a 5 % boundary.
- All statements are parameterized (`@p1`, `@p2`, …). Status and progress
  updates target the `queue_id` returned by the same query that selected the
  firmware, so a job enqueued mid-request is never updated by mistake.

## Download protocol

1. **Metadata.** `GET …?device_id=D&offset=-1` returns 20 raw bytes:

   | Bytes  | Content                                       |
   |--------|-----------------------------------------------|
   | 0–3    | firmware size, unsigned 32-bit little-endian  |
   | 4–19   | MD5 digest of the image, raw (not hex)        |

2. **Chunks.** `GET …?device_id=D&offset=O&block_size=B` returns the bytes
   `[O, min(O+B, size))` of the image. The device advances `O` by `B` after each
   response. The last chunk may be shorter than `B`.
3. **End of file.** When `O ≥ size`, the response is `text/plain` with the body
   `*** MD5 = <32 uppercase hex chars> ***` and the job status becomes `6`.
4. The device compares the MD5 of the received bytes with the metadata digest.

Requests are stateless and idempotent (apart from the status/progress columns),
so a device can retry any chunk or resume from any offset.

## API reference

All endpoints are `GET` (the local server also accepts `HEAD`).

### Health check

```
GET /health
GET /default/health
GET /default/DownloadIOTFirmware          (no query string)
```

Response `200 text/plain`, body `OK`. Any request without `device_id`,
`offset` and `block_size` is also answered as a health check, matching the
original behavior that the device simulator relies on.

### Firmware download

```
GET /default/DownloadIOTFirmware?device_id={id}&offset={offset}&block_size={size}
```

| Parameter    | Required | Rules                                                                                  |
|--------------|----------|----------------------------------------------------------------------------------------|
| `device_id`  | yes      | 1–64 characters from `A-Z a-z 0-9 - _ . :`                                             |
| `offset`     | no       | `-1` (metadata) or an integer `0 … 4294967295`. Absent/empty means `0`.               |
| `block_size` | no       | Integer `1 … MAX_BLOCK_SIZE` (default limit 524288). Absent/empty means `1024`. Ignored when `offset=-1`. |

Other query parameters are ignored. Repeating a parameter with different values
is rejected.

| Status | When                                                                 | Body (text/plain unless noted)                  |
|--------|----------------------------------------------------------------------|-------------------------------------------------|
| 200    | metadata                                                             | 20 bytes, `application/octet-stream`            |
| 200    | chunk                                                                | raw bytes, `application/octet-stream`           |
| 200    | `offset` ≥ size                                                      | `*** MD5 = <hex> ***`                           |
| 400    | invalid or missing parameter                                         | e.g. `invalid offset: must be an integer`       |
| 404    | no firmware job for the device, or its firmware is missing           | `No pending firmware for device {id}`           |
| 500    | database not configured, query failure, invalid firmware record      | generic message, details only in logs           |
| 503    | database timeout / request cancelled                                 | generic message, `Retry-After: 1`               |

Chunk responses carry these headers:

| Header                | Example                                        |
|-----------------------|------------------------------------------------|
| `Content-Length`      | `1024`                                         |
| `Content-Disposition` | `attachment; filename="firmware_12345_part.bin"` |
| `X-Firmware-MD5`      | `9E107D9D372BB6826BD81D3542A419D6`             |
| `X-Firmware-Size`     | `48213`                                        |

Examples (local server):

```bash
curl -o meta.bin  "http://localhost:8080/default/DownloadIOTFirmware?device_id=12345&offset=-1"
curl -o part0.bin "http://localhost:8080/default/DownloadIOTFirmware?device_id=12345&offset=0&block_size=1024"
curl              "http://localhost:8080/default/DownloadIOTFirmware?device_id=12345&offset=999999"
```

## Configuration

| Variable                | Default   | Description                                                                 |
|-------------------------|-----------|-----------------------------------------------------------------------------|
| `DB_CONNECTION_STRING`  | —         | **Required** for firmware requests. go-mssqldb format, e.g. `sqlserver://user:pass@host:1433?database=db&encrypt=true`. Never logged. |
| `DB_MAX_OPEN_CONNS`     | `4`       | Max open connections per execution environment (1–100).                    |
| `DB_MAX_IDLE_CONNS`     | `2`       | Max idle connections kept for warm invocations (0–100, ≤ max open).        |
| `DB_CONN_MAX_LIFETIME`  | `5m`      | Recycle connections after this duration (Go duration syntax).              |
| `DB_CONN_MAX_IDLE_TIME` | `1m`      | Close connections idle for longer than this.                               |
| `DB_QUERY_TIMEOUT`      | `10s`     | Upper bound for the database work of a single request.                     |
| `MAX_BLOCK_SIZE`        | `524288`  | Largest accepted `block_size` in bytes (1 – 4194304).                      |
| `LOCAL_ENVIRONMENT`     | —         | `true` starts the local HTTP server instead of the Lambda runtime.         |
| `PORT`                  | `8080`    | Local server port.                                                         |

Invalid optional values are logged and replaced by the default.

**Why 512 KiB?** Base64 grows the body by 4/3, so a 512 KiB block is ~683 KiB on
the wire: under the 1 MB ALB → Lambda response limit and well under the 6 MB
synchronous Lambda payload limit. Requests above the limit are rejected (not
truncated), because clients advance their offset by the *requested* block size.

## Running locally

Requirements: Go (version in `go.mod`), a reachable SQL Server with the tables
above for firmware requests. Health checks and validation work without a
database.

**Windows**

```bat
rem optional: put "set DB_CONNECTION_STRING=..." in local.env.bat (git-ignored)
rodar_local.bat
```

**Any platform**

```bash
export LOCAL_ENVIRONMENT=true
export DB_CONNECTION_STRING='sqlserver://user:pass@localhost:1433?database=firmware'
go run .
```

The server listens on `http://localhost:8080` and registers:

| Route                           | Purpose                                        |
|---------------------------------|------------------------------------------------|
| `/health`, `/default/health`    | health check                                   |
| `/default/DownloadIOTFirmware`  | firmware download (canonical)                  |
| `/default/download_firmware`    | alias (route previously documented here)       |
| `/default/DownloadFirmware`     | alias (route previously used by the simulator) |

The local adapter forwards the request context, method, headers and query
string (single- and multi-value, like API Gateway), decodes base64 bodies,
preserves status codes and rejects methods other than `GET`/`HEAD` with `405`.
Logs are JSON lines on stdout.

### Device simulator

`teste/` contains a stand-alone client that performs the full protocol, saves
the image to `firmware_download.bin` and verifies the MD5 (exit code 1 on
mismatch).

```bash
cd teste
go run . 12345                    # localhost, 1024-byte blocks
go run . 12345 4096               # localhost, 4096-byte blocks
go run . 12345 1024 https://your-api.example.com/default/DownloadIOTFirmware
```

On Windows, `rodar_teste.bat` prompts for the same values.

## Testing

```bash
go test ./...
go test -race ./...     # requires CGO (a C compiler) on Windows
go vet ./...
```

The tests do not need SQL Server. They use an in-memory `firmwareStore` for the
handler and a scripted `database/sql` driver for the SQL layer, and cover:
metadata encoding, chunk boundaries, EOF, empty firmware, missing firmware,
parameter validation (offset, block size, device ID, duplicates, overflow),
corrupted records, database failures and timeouts, status/progress update
failures, progress calculation, full and resumed downloads with MD5
verification, concurrent downloads, connection release, and the local HTTP
adapter.

GitHub Actions (`.github/workflows/ci.yml`) runs gofmt, `go mod tidy` check,
`go vet`, `go test -race`, builds the simulator and the Linux Lambda binary,
and fails if build artifacts are tracked. It never deploys and needs no AWS
credentials.

## Building and deploying

```bat
build_lambda.bat
```

or, on Linux/macOS:

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -tags lambda.norpc -ldflags="-s -w" -o bootstrap .
zip lambda.zip bootstrap
```

`build_lambda.bat` packages with AWS's `build-lambda-zip` tool so that
`bootstrap` keeps its executable bit (the built-in Windows `tar -a` does not).
`bootstrap` and `lambda.zip` are git-ignored; never commit them.

Deployment prerequisites (manual, not automated by this repository):

- Lambda runtime `provided.al2023` (or `provided.al2`), architecture `x86_64`, handler `bootstrap`.
- Network path from the Lambda to SQL Server (VPC subnets / security groups) and TLS on the SQL connection (`encrypt=true`).
- `DB_CONNECTION_STRING` supplied from AWS Secrets Manager or SSM Parameter Store, not typed into the console in plain text.
- A database login restricted to `SELECT` on both tables and `UPDATE (status, progress)` on `dbo.device_firmware_queue`.
- API Gateway: binary media type `application/octet-stream` (REST API) so base64 bodies are decoded; or an ALB target group with Lambda multi-value headers disabled.
- Lambda timeout greater than `DB_QUERY_TIMEOUT`.

## Security assumptions

This service **does not authenticate or authorize callers**. Anyone who can
reach the endpoint and knows or guesses a `device_id` can download that
device's firmware and change its job's `status`/`progress`. The following are
therefore deployment **requirements**, enforced upstream:

1. **Authentication of devices** at API Gateway / ALB (for example mutual TLS
   with per-device certificates, a Lambda authorizer, or IAM/SigV4), and a
   binding between the authenticated identity and the `device_id` parameter.
2. **TLS** for all client traffic.
3. **Rate limiting / throttling** (API Gateway usage plans or AWS WAF), since
   each chunk request runs a SQL query and hashes the full image.
4. **Least-privilege database credentials** stored in a secrets manager.

What the service itself guarantees:

- `device_id`, `offset` and `block_size` are validated before any SQL is run,
  and all SQL is parameterized.
- Internal errors (SQL messages, server names) are logged but never returned to
  clients; the connection string is never logged.
- `block_size` is bounded, so a request cannot force unbounded memory use.

## Known limitations

These behaviors are inherited from the original implementation and kept for
compatibility with existing devices. Changing them needs a coordinated decision.

- **Job selection ignores status.** The latest `command_type = 221` job is
  served even if it is already complete, and requesting metadata again resets
  the status from `6` back to `5`.
- **Completion is inferred from the offset.** Any request with
  `offset ≥ size` marks the job as `6`, even if the device skipped chunks. The
  service cannot tell whether the device verified or installed the image.
- **Progress is computed from the requested window** (`offset + block_size`),
  capped at 100, and can move backwards when several transfers for the same
  device interleave. It is advisory: a failed progress write is logged and the
  chunk is still served. A failed **status** write returns 500 so the device
  retries and the transition is not lost.
- **`/default/DownloadIOTFirmware` is a health path in Lambda.** If the
  integration passes that exact string as `Path` (e.g. ALB, or an HTTP API with
  payload v1.0 on the `default` stage), every request on it — including
  downloads — returns `OK`. This is preserved because load balancer health checks
  may depend on it; production downloads must use an integration where the
  event `Path` differs (e.g. API Gateway REST, where `Path` excludes the stage).
  The local server does not forward `Path` on download routes for this reason.
- **The EOF marker is in-band.** A device that ignores `Content-Type` cannot
  distinguish a binary chunk starting with `*** MD5` from the end marker. The
  bundled simulator checks `Content-Type: text/plain`.
- **Every chunk request hashes the whole image** in SQL Server
  (`HASHBYTES('MD5', …)`) to fill `X-Firmware-MD5`. Large images and many devices
  increase database CPU; caching by `firmware_id` would remove this.
- **MD5 is an integrity check, not authenticity.** It detects corruption, not
  tampering; firmware should also be signed and verified on the device.
- **4 GiB limit.** The size field is 32 bits; larger images are rejected with 500.
- The Lambda handler does not check the HTTP method; restrict the route to `GET`
  in API Gateway.

## Project layout

```
.
├── main.go, handler.go, params.go, store.go, config.go, local.go
├── *_test.go                  # unit tests (no database needed)
├── teste/                     # device simulator (separate module)
├── build_lambda.bat           # Windows build + packaging
├── rodar_local.bat            # Windows: run local server
├── rodar_teste.bat            # Windows: run simulator
└── .github/workflows/ci.yml   # tests and static checks
```

## License

MIT — see [LICENSE](LICENSE).
