@echo off
rem ============================================================
rem  Pandora 后端一键启动器(双击即用)
rem ------------------------------------------------------------
rem  双击本文件:默认本机 local 模式(基础设施 docker + go 服务宿主进程),
rem             策划本地联调首选,可在 VS Code 断点调试。
rem  命令行用法(可传参,转发给 start.ps1):
rem     start.cmd -Mode docker
rem     start.cmd -Mode k8s
rem     start.cmd -Mode local -Profile match
rem     start.cmd -Status
rem     start.cmd -Check
rem     start.cmd -Mode docker -Down
rem ============================================================
setlocal
cd /d "%~dp0"

rem 优先 PowerShell 7(pwsh),没有则回退 Windows PowerShell
where pwsh >nul 2>nul && (set "PS=pwsh") || (set "PS=powershell")

%PS% -NoProfile -ExecutionPolicy Bypass -File "%~dp0tools\scripts\start.ps1" %*
set "RC=%ERRORLEVEL%"

rem 双击(无参数)时停住窗口,方便看输出
if "%~1"=="" pause
exit /b %RC%
