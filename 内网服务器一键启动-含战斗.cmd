@echo off
chcp 65001 >nul
rem ============================================================
rem  Pandora backend  intranet one-click start (with battle)  (double-click to run)
rem ------------------------------------------------------------
rem  This machine acts as the intranet test server + battle build:
rem  infra + 17 business services run in docker; ds/hub allocator run as host go
rem  processes; after a match is made the host allocator launches the Windows DS.
rem  It also auto-rewrites the Hub/Battle DS address returned to clients to this
rem  machine LAN IP, so other planner clients on the LAN can join the real
rem  lobby + battle.
rem
rem  Prerequisites (the script self-checks and prompts):
rem    Go (only to build the 2 allocators) + Docker + mkcert + a UE-built Windows
rem    Server package (PandoraServer.exe, placed in the sibling Client dir next to
rem    this repo, auto-detected).
rem  Planner machines need no Docker/Go, just LAN access and trust of the same dev
rem  CA (mkcert root cert).
rem
rem  To stop: double-click the intranet one-click stop .cmd.
rem ============================================================
setlocal
cd /d "%~dp0"

rem This machine does not build code, only runs the offline package: force fully
rem offline, docker load the offline images directly, skip all docker build,
rem no network, immune to Docker DNS flakiness. To temporarily build latest code,
rem do it on a dev machine or pass -Rebuild manually.
set "PANDORA_OFFLINE=1"

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

%PS% -NoProfile -ExecutionPolicy Bypass -File "%~dp0tools\scripts\play.ps1" -Battle -Intranet
set "RC=%ERRORLEVEL%"

pause
exit /b %RC%