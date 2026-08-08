@echo off
echo ==================================================
echo    Firmware download test
echo ==================================================
echo.
echo  1 - Test on localhost (local server)
echo  2 - Test on AWS (firmware.example.com)
echo  3 - Test via direct IP (203.0.113.10:3020)
echo.

set /p OPCAO="Choose (1, 2 or 3): "
set /p ID_TERMINAL="Enter device ID: "
set /p TAMANHO_BLOCO="Block size (Enter for 1024): "

if "%TAMANHO_BLOCO%"=="" set TAMANHO_BLOCO=1024

echo.

cd teste

if "%OPCAO%"=="3" (
    echo Testing via direct IP...
    go run main.go %ID_TERMINAL% %TAMANHO_BLOCO% ip
) else if "%OPCAO%"=="2" (
    echo Testing against AWS...
    go run main.go %ID_TERMINAL% %TAMANHO_BLOCO% aws
) else (
    echo Testing against localhost...
    go run main.go %ID_TERMINAL% %TAMANHO_BLOCO%
)

pause
