@echo off
setlocal EnableExtensions
chcp 65001 >nul

set "PAUSE_ON_EXIT=0"
if "%~1"=="" set "PAUSE_ON_EXIT=1"

if not "%~3"=="" goto usage

set "PROFILE=%~1"
if not defined PROFILE set "PROFILE=pandora-agones"

set "RESTART=0"
if "%~2"=="" goto run
if /I "%~2"=="restart" goto enable_restart
if /I "%~2"=="--restart" goto enable_restart
goto usage

:enable_restart
set "RESTART=1"
goto run

:run
set "PS_SCRIPT=%~dp0reset_data_service_schema.ps1"
if not exist "%PS_SCRIPT%" (
    echo [失败] 找不到 PowerShell 脚本：%PS_SCRIPT%
    set "EXIT_CODE=1"
    goto finish
)

if "%RESTART%"=="1" (
    pwsh.exe -NoProfile -ExecutionPolicy Bypass -File "%PS_SCRIPT%" -Mode k8s -MinikubeProfile "%PROFILE%" -Restart -Confirm
) else (
    pwsh.exe -NoProfile -ExecutionPolicy Bypass -File "%PS_SCRIPT%" -Mode k8s -MinikubeProfile "%PROFILE%" -Confirm
)
set "EXIT_CODE=%ERRORLEVEL%"
goto finish

:usage
echo 用法：%~nx0 [minikube-profile] [restart^|--restart]
echo 示例：%~nx0 pandora-agones
echo 示例：%~nx0 pandora-agones restart
set "EXIT_CODE=64"

:finish
if not "%EXIT_CODE%"=="0" echo [失败] 脚本退出码：%EXIT_CODE%
if "%PAUSE_ON_EXIT%"=="1" pause
exit /b %EXIT_CODE%
