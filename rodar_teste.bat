@echo off
setlocal
echo ==================================================
echo    Firmware download test (device simulator)
echo ==================================================
echo.
echo  1 - Local server (http://localhost:8080)
echo  2 - Custom URL
echo.

set /p OPCAO="Choose (1 or 2): "
set /p ID_TERMINAL="Enter device ID: "
set /p TAMANHO_BLOCO="Block size (Enter for 1024): "

if "%TAMANHO_BLOCO%"=="" set TAMANHO_BLOCO=1024

echo.

cd /d "%~dp0teste"

if "%OPCAO%"=="2" (
    set /p URL_CUSTOM="Download URL (e.g. https://your-api.example.com/default/DownloadIOTFirmware): "
    call go run . %ID_TERMINAL% %TAMANHO_BLOCO% %%URL_CUSTOM%%
) else (
    echo Testing against localhost...
    go run . %ID_TERMINAL% %TAMANHO_BLOCO%
)

endlocal
pause
