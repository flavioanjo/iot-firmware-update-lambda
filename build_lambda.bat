@echo off
setlocal
echo ==================================================
echo    Building Lambda for AWS (Linux/amd64)...
echo ==================================================

cd /d "%~dp0"

set GOOS=linux
set GOARCH=amd64
set CGO_ENABLED=0

rem -trimpath removes local file system paths from the binary.
rem -tags lambda.norpc drops the legacy RPC mode, unused on provided.al2/al2023.
go build -trimpath -tags lambda.norpc -ldflags="-s -w" -o bootstrap .

if %ERRORLEVEL% NEQ 0 (
    echo ERROR: Build failed!
    pause
    exit /b 1
)

echo Build OK: bootstrap
echo Generating lambda.zip...

if exist lambda.zip del /q lambda.zip
rem build-lambda-zip (published by AWS in aws-lambda-go) marks bootstrap as
rem executable inside the archive. "tar -a" on Windows does not, which makes
rem the custom runtime fail with "permission denied" in Lambda.
rem The packaging tool runs on this machine, so clear the cross-compile target.
set GOOS=
set GOARCH=
go run github.com/aws/aws-lambda-go/cmd/build-lambda-zip@v1.54.0 -o lambda.zip bootstrap

if %ERRORLEVEL% NEQ 0 (
    echo ERROR: Packaging failed!
    pause
    exit /b 1
)

echo ==================================================
echo    Ready! File: lambda.zip  (git-ignored, do not commit)
echo ==================================================

endlocal
pause
