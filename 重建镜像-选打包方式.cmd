@echo off
chcp 936 >nul
rem ============================================================
rem  Pandora 后端  重建业务镜像 - 选打包方式(开发机双击即用)
rem ------------------------------------------------------------
rem  两种打包方式二选一:
rem    [1] 宿主编译(方案B,推荐): 本机 Go 交叉编译, 秒级增量, 需装 Go
rem    [2] 容器内编译(方案A): 在 Docker 里编译, 无需本机 Go, 冷缓存较慢
rem  可只重建单个服务(如 battle-result), 也可重建全部业务镜像。
rem ============================================================
setlocal
cd /d "%~dp0"

rem 本项目脚本要求 PowerShell 7(pwsh)。缺失则明确报错退出, 不回退 Windows PowerShell 5.1。
where pwsh >nul 2>nul
if errorlevel 1 (
  echo.
  echo  [ERR] 未找到 PowerShell 7 pwsh。本脚本需要 PowerShell 7。
  echo        下载安装: https://aka.ms/powershell  或  winget install Microsoft.PowerShell
  echo.
  pause
  exit /b 1
)
set "PS=pwsh"

echo.
echo  请选择打包方式:
echo    [1] 宿主编译(方案B,推荐,快): 本机 Go 交叉编译再塞进 scratch 镜像
echo    [2] 容器内编译(方案A): 在 Docker 容器里 go build
echo.
set "MODE=host"
set /p "CHOICE= 输入 1 或 2 后回车(默认 1=宿主编译): "
if "%CHOICE%"=="2" set "MODE=incontainer"

echo.
echo  要重建哪些服务?
echo    直接回车 = 全部业务镜像
echo    或输入单个服务名(连字符名), 例如: battle-result / login / matchmaker
set /p "SVC= 服务名(留空=全部): "

echo.
if "%SVC%"=="" (
  echo  即将用 %MODE% 方式重建【全部】业务镜像...
  %PS% -NoProfile -ExecutionPolicy Bypass -File "%~dp0tools\scripts\start.ps1" -Mode docker -BuildOnly -Rebuild -BuildMode %MODE%
) else (
  echo  即将用 %MODE% 方式重建【%SVC%】...
  %PS% -NoProfile -ExecutionPolicy Bypass -File "%~dp0tools\scripts\start.ps1" -Mode docker -BuildOnly -Rebuild -BuildMode %MODE% -Only %SVC%
)
set "RC=%ERRORLEVEL%"

echo.
if "%RC%"=="0" (
  echo  [OK] 镜像重建完成。重启对应容器即可用上新镜像:
  echo       docker compose -f deploy\docker-compose.services.yml up -d %SVC%
) else (
  echo  [ERR] 重建失败,返回码 %RC%,请看上方日志。
)

pause
exit /b %RC%
