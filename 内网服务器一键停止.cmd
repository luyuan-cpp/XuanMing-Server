@echo off
chcp 65001 >nul
rem ============================================================
rem  Pandora backend  intranet one-click stop (double-click to run)
rem ------------------------------------------------------------
rem  Stops the whole backend started by the intranet one-click start .cmd
rem  (17 business containers + 2 host allocators + local Windows DS).
rem  Data volumes (MySQL/Redis etc.) are kept; data persists for next start.
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

%PS% -NoProfile -ExecutionPolicy Bypass -File "%~dp0tools\scripts\play.ps1" -Battle -Stop
set "RC=%ERRORLEVEL%"

pause
exit /b %RC%