📦 IoT Firmware Update Lambda
AWS Lambda implementation for delivering IoT firmware updates using chunked transfer, MD5 validation, and SQL Server metadata.
Includes a local test program located in the teste/ folder.

📘 Overview
This project provides a serverless firmware distribution service for IoT devices.
It is built using:

AWS Lambda (Go)

API Gateway

SQL Server

Custom runtime (bootstrap)

The Lambda exposes a simple HTTP interface that allows IoT devices to:

Request firmware metadata

Download firmware in chunks

Validate integrity using MD5

Track update progress

A local test program is included to simulate the device behavior.

🏗️ Architecture
Firmware Flow
Device requests metadata using offset=-1  
→ receives firmware size + raw MD5

Device downloads chunks using:
offset=0, offset=1024, offset=2048, …

Lambda returns each chunk in Base64

When the offset exceeds the firmware size, Lambda returns the final MD5

Device compares MD5 to ensure firmware integrity

📁 Project Structure
Código
iot-firmware-update-lambda/
│
├── main.go               # Lambda bootstrap + local server
├── handler.go            # Core firmware logic (HandleRequest)
│
├── bootstrap             # Custom runtime for AWS Lambda
├── lambda.zip            # Packaged Lambda ready for upload
│
├── build_lambda.bat      # Windows build script
├── rodar_local.bat       # Run Lambda locally
├── rodar_teste.bat       # Run local test program
│
├── teste/                # Local test program (simulates IoT device)
│   └── main.go
│
├── go.mod
├── go.sum
├── LICENSE
└── README.md
🚀 Running Locally
1. Build the Lambda
Código
build_lambda.bat
This generates:

Código
lambda.zip
2. Run the Lambda locally
Código
rodar_local.bat
The local server will expose:

Código
http://localhost:8080/default/download_firmware
http://localhost:8080/default/health
🧪 Running the Test Program (IoT Device Simulator)
The test program is located in:

Código
teste/main.go
To run it:

Código
rodar_teste.bat
Or manually:

Código
go run teste/main.go <device_id> [block_size] [primary|secondary]
Examples:

Código
go run teste/main.go 12345
go run teste/main.go 12345 2048
go run teste/main.go 12345 1024 primary
The test program:

Requests metadata

Downloads firmware in chunks

Tracks progress

Validates MD5

Prints detailed logs

🔌 Endpoints
Health Check
Código
GET /default/health
Metadata (offset = -1)
Código
GET /default/download_firmware?device_id=123&offset=-1
Returns:

4 bytes: firmware size

16 bytes: raw MD5

Base64 encoded

Chunk Download
Código
GET /default/download_firmware?device_id=123&offset=2048&block_size=1024
Returns:

Base64 chunk

Headers:

X-Firmware-MD5

X-Firmware-Size

🗄️ Database
Tables
firmware_catalog

device_firmware_queue

Fields
firmware_binary

firmware_id

device_id

status

progress

Command Type
Código
FirmwareUpdateCommand = 1001
🔐 Security
No real endpoints

No sensitive data

SQL Server queries fully parameterized

All identifiers in English

Generic IoT terminology

📦 Deployment
Upload lambda.zip to AWS Lambda using:

Runtime: provided.al2

Handler: bootstrap

API Gateway: HTTP API or REST API

📄 License
MIT License.