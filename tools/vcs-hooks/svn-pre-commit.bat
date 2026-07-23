@echo off
rem Pandora 客户端 SVN 仓库服务端 pre-commit 钩子(VisualSVN Server / Windows 版)。
rem 部署:拷到 <仓库>\hooks\pre-commit.bat,同目录放 svn-pre-commit.ps1。
rem VisualSVN 也可在管理台 Repository Properties -> Hooks -> Pre-commit 里粘贴本文件内容。
setlocal
set REPOS=%1
set TXN=%2
powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0svn-pre-commit.ps1" -Repos %REPOS% -Txn %TXN%
exit /b %errorlevel%
