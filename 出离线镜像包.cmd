@echo off
chcp 65001 >nul
rem ============================================================
rem  Pandora 后端  出离线镜像包(在「能联网的开发机」上双击即用)
rem ------------------------------------------------------------
rem  重新构建最新的 17 个业务镜像,并打包成:
rem      deploy\offline-images\pandora-images.tar
rem  (覆盖同名文件)。
rem
rem  出完包后:svn commit 代码 + 这个 tar,内网机 svn update 后
rem  双击「内网服务器一键启动-含战斗.cmd」即用上新镜像(纯离线,不再联网构建)。
rem
rem  前置:这台机器能联网(拉得到基础镜像 / go 模块)+ 装了 Docker。
rem ============================================================
setlocal
cd /d "%~dp0"

where pwsh >nul 2>nul && (set "PS=pwsh") || (set "PS=powershell")

%PS% -NoProfile -ExecutionPolicy Bypass -File "%~dp0tools\scripts\export_images.ps1" -Build
set "RC=%ERRORLEVEL%"

pause
exit /b %RC%
