@echo off
chcp 65001 >nul
rem ============================================================
rem  Pandora backend  rebuild business images - choose build mode (dev machine)
rem ------------------------------------------------------------
rem  Two build modes, pick one:
rem    [1] host build (plan B, recommended): local Go cross-compile, fast incremental, needs Go
rem    [2] in-container build (plan A): build inside Docker, no local Go, slow on cold cache
rem  Can rebuild a single service (e.g. battle-result) or all business images.
rem ============================================================
setlocal
cd /d "%~dp0"

rem This project requires PowerShell 7 (pwsh). If missing, error out clearly; do
rem not fall back to Windows PowerShell 5.1.
where pwsh >nul 2>nul
if errorlevel 1 (
  echo.
  echo  [ERR] PowerShell 7 pwsh not found. This script requires PowerShell 7.
  echo        Install: https://aka.ms/powershell  or  winget install Microsoft.PowerShell
  echo.
  pause
  exit /b 1
)
set "PS=pwsh"

echo.
echo  Choose build mode:
echo    [1] host build (plan B, recommended, fast): local Go cross-compile into scratch image
echo    [2] in-container build (plan A): go build inside a Docker container
echo.
set "MODE=host"
set /p "CHOICE= Enter 1 or 2 then press Enter (default 1=host build): "
if "%CHOICE%"=="2" set "MODE=incontainer"

echo.
echo  Which services to rebuild?
echo    Press Enter = all business images
echo    Or enter a single service name (hyphen name), e.g.: battle-result / login / matchmaker
set /p "SVC= Service name (empty=all): "

echo.
if "%SVC%"=="" (
  echo  Rebuilding [ALL] business images using %MODE% mode...
  %PS% -NoProfile -ExecutionPolicy Bypass -File "%~dp0tools\scripts\start.ps1" -Mode docker -BuildOnly -Rebuild -BuildMode %MODE%
) else (
  echo  Rebuilding [%SVC%] using %MODE% mode...
  %PS% -NoProfile -ExecutionPolicy Bypass -File "%~dp0tools\scripts\start.ps1" -Mode docker -BuildOnly -Rebuild -BuildMode %MODE% -Only %SVC%
)
set "RC=%ERRORLEVEL%"

echo.
if "%RC%"=="0" (
  echo  [OK] Image rebuild done. Restart the matching container to use the new image:
  echo       docker compose -f deploy\docker-compose.services.yml up -d %SVC%
) else (
  echo  [ERR] Rebuild failed, return code %RC%, see the log above.
)

pause
exit /b %RC%