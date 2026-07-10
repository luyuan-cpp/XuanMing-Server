@echo off
chcp 65001 >nul
rem ============================================================
rem  Pandora backend one-click launcher (double-click to run)
rem ------------------------------------------------------------
rem  Double-click: default local mode (infra in docker + go services as host
rem                processes), preferred for local dev, supports VS Code
rem                breakpoint debugging.
rem
rem  5 environments (DS allocation mode varies per env):
rem     local    local windows debug      DS=local (host exec Windows DS)
rem     docker   all-in-docker            DS=mock
rem     intranet intranet test (bind LAN IP) DS=mock
rem     k8s      local minikube+Agones    DS=agones (real Linux DS, prod-equivalent)
rem     online   online k8s cluster       DS=agones (-Env test / prod)
rem
rem  CLI usage (args forwarded to start.ps1):
rem     start.cmd -Mode docker
rem     start.cmd -Mode k8s
rem     start.cmd -Mode local
rem     start.cmd -Status
rem     start.cmd -Check
rem     start.cmd -Mode docker -Down
rem
rem  Quick resume / reset after reboot (see deploy/k8s/agones/README.md):
rem     start.cmd -Mode k8s -Resume      rem no rebuild, restore last state
rem     start.cmd -Mode k8s -Reset       rem minikube delete then fresh deploy
rem
rem  Real DS loop on this machine (minikube+Agones, no mock):
rem     start.cmd -Mode k8s              rem cluster+Agones+Fleet+20 deployments; auto Envoy bridge/UDP relay
rem
rem  Online real cluster (Fleet image/callbacks must be injected per env, fail-fast if missing):
rem     start.cmd -Mode online -Env test -TestKubeContext pandora-test -Registry registry.mycorp.com -Tag v1.2.3 ^
rem        -BattleDsImage registry.mycorp.com/pandora/battle-ds:v1.2.3 ^
rem        -HubDsImage    registry.mycorp.com/pandora/hub-ds:v1.2.3 ^
rem        -DsGatewayAddr pandora-envoy.pandora.svc:8444
rem ============================================================
setlocal
cd /d "%~dp0"

rem Prefer PowerShell 7 (pwsh), fall back to Windows PowerShell if missing
where pwsh >nul 2>nul && (set "PS=pwsh") || (set "PS=powershell")

%PS% -NoProfile -ExecutionPolicy Bypass -File "%~dp0tools\scripts\start.ps1" %*
set "RC=%ERRORLEVEL%"

rem When double-clicked (no args) keep the window open to read output
if "%~1"=="" pause
exit /b %RC%
