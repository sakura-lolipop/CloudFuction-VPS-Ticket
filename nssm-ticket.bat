@echo off
REM ============================================================
REM  Hotify CF 2.0 cf-ticket NSSM installer（铸票厂，Windows VPS）。
REM  Run ONCE on the VPS as ADMINISTRATOR。自包含假设：
REM    本目录 = cf-ticket.exe + private.json（SA，含 PRIVATE KEY 自动扫描）
REM    nssm.exe 已在系统或 go-harmony 目录（路径不同则改 NSSM 变量）
REM
REM  Tunnel 不装新的：复用现有 HotifyTunnel，其 config.yml 加一条
REM  ingress 规则（ticket.hotify.love -> http://localhost:8091）后
REM  重启 HotifyTunnel 服务即生效。
REM
REM  Re-install (after code update):
REM    nssm stop HotifyTicketCF && nssm remove HotifyTicketCF confirm
REM    然后重跑本脚本。
REM ============================================================

set NSSM=nssm.exe
cd /d "%~dp0"

if not exist "cf-ticket.exe" (
    echo [ERROR] cf-ticket.exe not found in this folder.
    pause
    exit /b
)
if not exist "private.json" (
    echo [ERROR] private.json not found in this folder ^(SA, AGC -^> 项目设置 -^> 服务账号^).
    pause
    exit /b
)
if not exist "logs" mkdir logs

echo --- Installing HotifyTicketCF (cf-ticket, anonymous mode) ---
"%NSSM%" install HotifyTicketCF "%cd%\cf-ticket.exe"
"%NSSM%" set HotifyTicketCF AppDirectory "%cd%"
REM env 全默认即匿名开放 + TTL 600；需要收紧时改这里（TICKET_AUTH_TOKEN / TICKET_RATE_LIMIT_IP 等）
"%NSSM%" set HotifyTicketCF AppEnvironmentExtra PORT=8091 TICKET_TTL_SECONDS=600
"%NSSM%" set HotifyTicketCF Start SERVICE_AUTO_START
"%NSSM%" set HotifyTicketCF AppStdout "%cd%\logs\nssm-ticket.log"
"%NSSM%" set HotifyTicketCF AppStderr "%cd%\logs\nssm-ticket.log"
"%NSSM%" set HotifyTicketCF AppRotateFiles 1
"%NSSM%" set HotifyTicketCF AppRotateBytes 5242880

echo --- Starting ---
"%NSSM%" start HotifyTicketCF
"%NSSM%" status HotifyTicketCF
echo.
echo Done. curl http://localhost:8091/ to verify (3-field JSON).
pause
