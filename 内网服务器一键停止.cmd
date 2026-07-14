@echo off
chcp 65001 >nul
rem ============================================================
rem  Pandora backend  intranet one-click stop (double-click to run)
rem ------------------------------------------------------------
rem  The battle mode (with-battle one-click start) was RETIRED on 2026-07-14:
rem  Windows DS now only runs in local mode (start.ps1 -Mode local, debugging);
rem  intranet servers use the k8s one-click start/stop .cmd (Agones Linux DS).
rem  See docs/design/decision-revisit-retire-battle-mode.md.
rem  This script is kept ONLY to clean up a leftover battle stack on machines
rem  that ran the old with-battle build (17 business containers + 2 host
rem  allocators + local Windows DS). Data volumes (MySQL/Redis etc.) are kept.
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