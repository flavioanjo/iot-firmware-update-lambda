@echo off
setlocal
echo ==================================================
echo    Starting the firmware service in local mode...
echo ==================================================

rem Optional, git-ignored file with local settings, for example:
rem   set DB_CONNECTION_STRING=sqlserver://user:password@localhost:1433?database=firmware
if exist "%~dp0local.env.bat" call "%~dp0local.env.bat"

if "%DB_CONNECTION_STRING%"=="" (
    echo WARNING: DB_CONNECTION_STRING is not set.
    echo          Health checks will work, firmware requests will return HTTP 500.
    echo          Set it in this window or in local.env.bat ^(git-ignored^).
    echo.
)

set LOCAL_ENVIRONMENT=true

cd /d "%~dp0"
go run .

endlocal
pause
