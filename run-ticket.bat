@echo off
REM ============================================================
REM  Hotify CF 2.0 cf-ticket launcher（前台窗口，同 run.bat 风格）。
REM  前提：本目录有 private.json（SA，启动自动扫描）。
REM  可选加固（删掉行首 REM 启用，详见 docs/cloudfuctionticketenv.md）：
REM    set TICKET_RATE_LIMIT_IP=100
REM    set TICKET_AUTO_BAN=5
REM  注意：窗口关了服务就停；VPS 重启后要重新双击本文件。
REM  （想开机自启/崩溃自动拉起：改用 nssm-ticket.bat 装 Windows 服务）
REM ============================================================

cd /d "%~dp0"

if not exist "cf-ticket.exe" (
    echo [ERROR] cf-ticket.exe not found in this folder.
    pause
    exit /b
)
if not exist "private.json" (
    echo [ERROR] private.json not found in this folder ^(AGC -^> 项目设置 -^> 服务账号^).
    pause
    exit /b
)

set PORT=8091
set TICKET_TTL_SECONDS=600
echo Starting Hotify CF-Ticket (anonymous, ttl=600s, port 8091)...
cf-ticket.exe
pause
