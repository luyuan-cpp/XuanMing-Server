@echo off
chcp 65001 >nul
rem ============================================================
rem  Pandora backend  export offline image package (run on an online dev machine)
rem ------------------------------------------------------------
rem  Rebuilds the latest 20 business images (host go build by default) and packs
rem  them into:
rem      deploy\offline-images\pandora-images.tar
rem  (overwrites the same-named file).
rem
rem  After exporting: svn commit the code + this tar; on the intranet machine run
rem  svn update, then double-click the intranet one-click start .cmd to use the
rem  new images (fully offline, no network build).
rem
rem  Prerequisite: Go 1.26.5+ + Docker. Network is needed when local image/module caches are incomplete.
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

%PS% -NoProfile -ExecutionPolicy Bypass -File "%~dp0tools\scripts\export_images.ps1" -Build
set "RC=%ERRORLEVEL%"

pause
exit /b %RC%
