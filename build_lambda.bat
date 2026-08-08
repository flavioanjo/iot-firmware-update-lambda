@echo off
echo ==================================================
echo    Building Lambda for AWS (Linux/amd64)...
echo ==================================================

set GOOS=linux
set GOARCH=amd64
set CGO_ENABLED=0

go build -o bootstrap .

if %ERRORLEVEL% NEQ 0 (
    echo ERROR: Build failed!
    pause
    exit /b 1
)

echo Build OK: bootstrap
echo Generating lambda.zip...

tar -a -c -f lambda.zip bootstrap

echo ==================================================
echo    Ready! File: lambda.zip
echo ==================================================

pause
