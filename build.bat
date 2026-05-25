@echo off
setlocal enabledelayedexpansion

REM ==================== Version Configuration ====================
set VERSION=1.0.3

REM ==================== Project Paths ====================
set PROJECT_DIR=%~dp0
set PROJECT_DIR=%PROJECT_DIR:~0,-1%
set BUILD_DIR=%PROJECT_DIR%\build
set CLIENT_DIR=%PROJECT_DIR%\client
set SERVER_DIR=%PROJECT_DIR%\server

REM ==================== Auto-detect Build Info ====================
for /f "tokens=3" %%i in ('go version') do set GO_VER=%%i
REM Build timestamp in ISO format without spaces or colons (PowerShell for reliability)
for /f "usebackq" %%i in (`powershell -NoProfile -Command "Get-Date -Format 'yyyy-MM-dd_HHmmss'"`) do set BUILD_TIMESTAMP=%%i

echo ============================================
echo   TianXing Tunnel Build Script
echo   Version:  %VERSION%
echo   Go:       %GO_VER%
echo   Build at: %BUILD_TIMESTAMP%
echo ============================================
echo.

REM ==================== Clean Build Directory ====================
if exist "%BUILD_DIR%" (
    echo [1/4] Cleaning build directory...
    rmdir /s /q "%BUILD_DIR%"
)
mkdir "%BUILD_DIR%"
echo [1/4] Build directory created.

REM ==================== Build All Targets ====================
echo [2/4] Building binaries...

set COUNT=0
set TOTAL=9
set FAILED=0

call :build_target linux amd64 "" linux-amd64
call :build_target linux 386 "" linux-i386
call :build_target linux arm 6 linux-armhf
call :build_target linux arm64 "" linux-arm64
call :build_target windows amd64 "" windows-amd64
call :build_target windows 386 "" windows-i386
call :build_target windows arm64 "" windows-arm64
call :build_target darwin arm64 "" darwin-arm64
call :build_target darwin amd64 "" darwin-amd64

echo.
echo [3/4] Copying config files...

REM ==================== Package Archives ====================
echo [4/4] Creating archives...

where 7z >nul 2>&1
if %ERRORLEVEL%==0 (
    set ARCHIVER=7z
) else (
    set ARCHIVER=tar
)

pushd "%BUILD_DIR%"
for /d %%D in (*) do (
    if "!ARCHIVER!"=="7z" (
        7z a -tzip "%%D.zip" "%%D\*" >nul 2>&1
    ) else (
        tar -a -c -f "%%D.zip" "%%D"
    )
    echo   Created: %%D.zip
)
popd

echo.
echo ============================================
echo   Build complete! (%FAILED% failed)
echo   Output directory: %BUILD_DIR%
echo ============================================

endlocal
exit /b %FAILED%

REM ==================== Build Function ====================
:build_target
set T_GOOS=%~1
set T_GOARCH=%~2
set T_GOARM=%~3
set TAG=%~4

set /a COUNT+=1
echo   [%COUNT%/%TOTAL%] Building %TAG% ...

set TARGET_DIR=%BUILD_DIR%\%TAG%
set SERVER_OUT=%TARGET_DIR%\server
set CLIENT_OUT=%TARGET_DIR%\client

mkdir "%SERVER_OUT%"
mkdir "%CLIENT_OUT%"

REM Determine binary names
if "%T_GOOS%"=="windows" (
    set SERVER_BIN=tianxing-server.exe
    set CLIENT_BIN=tianxing-client.exe
) else (
    set SERVER_BIN=tianxing-server
    set CLIENT_BIN=tianxing-client
)

REM Set cross-compilation environment
set CGO_ENABLED=0
set GOOS=%T_GOOS%
set GOARCH=%T_GOARCH%
if not "%T_GOARM%"=="" set GOARM=%T_GOARM%

REM Build server
pushd "%SERVER_DIR%"
go build -ldflags "-s -w -X main.Version=%VERSION% -X main.BuildTime=%BUILD_TIMESTAMP% -X main.GoVersion=%GO_VER%" -o "%SERVER_OUT%\%SERVER_BIN%" .
if !ERRORLEVEL! neq 0 (
    popd
    echo   [ERROR] Failed to build server for %TAG%
    set /a FAILED+=1
    exit /b 0
)
popd

REM Build client
pushd "%CLIENT_DIR%"
go build -ldflags "-s -w -X main.Version=%VERSION% -X main.BuildTime=%BUILD_TIMESTAMP% -X main.GoVersion=%GO_VER%" -o "%CLIENT_OUT%\%CLIENT_BIN%" .
if !ERRORLEVEL! neq 0 (
    popd
    echo   [ERROR] Failed to build client for %TAG%
    set /a FAILED+=1
    exit /b 0
)
popd

REM Copy config files (rename to tianxing.conf as default)
copy /y "%PROJECT_DIR%\server.conf" "%SERVER_OUT%\tianxing.conf" >nul 2>&1
copy /y "%PROJECT_DIR%\client.conf" "%CLIENT_OUT%\tianxing.conf" >nul 2>&1

echo   [%COUNT%/%TOTAL%] %TAG% done.

exit /b 0
