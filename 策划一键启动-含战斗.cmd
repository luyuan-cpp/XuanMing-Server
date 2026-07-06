@echo off
chcp 65001 >nul
rem ============================================================
rem  Pandora backend  planner one-click start (with battle)  (double-click to run)
rem ------------------------------------------------------------
rem  Full local battle build: join lobby, matchmake, enter battle DS for one match.
rem  Backend: 17 business services in docker, ds/hub allocator as host go processes;
rem  after a match is made the host allocator launches the local Windows DS
rem  (PandoraServer.exe) for the battle.
rem  (The all-in-docker build uses DS=mock and cannot run a real battle DS, so no
rem  separate entry script is provided for it.)
rem
rem  This is the fully-local one-click build: the script auto-prepares the env
rem  (auto winget-installs Go / Docker if missing), starts infra (docker), starts
rem  17 business service containers + 2 host allocators, and launches the battle DS.
rem
rem  Prerequisites (the script auto-checks and prompts clearly):
rem    1) Go (only to build the 2 allocators) and Docker Desktop: auto winget-installed
rem       if missing (you may need to reopen the terminal and rerun once).
rem    2) A UE-built Windows Server package (PandoraServer.exe). No manual path edit:
rem       put the client Client repo as a sibling of this server repo (same parent
rem       dir); the script auto-detects Packages\Server_Win64_*\WindowsServer\
rem       PandoraServer.exe and injects it. No config change across machines/drives.
rem
rem  To stop: double-click the planner one-click stop .cmd.
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

%PS% -NoProfile -ExecutionPolicy Bypass -File "%~dp0tools\scripts\play.ps1" -Battle
set "RC=%ERRORLEVEL%"

pause
exit /b %RC%