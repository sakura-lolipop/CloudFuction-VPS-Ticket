@echo off
REM ============================================================
REM  Hotify CF 2.0 cf-ticket launcher (foreground window, same
REM  style as go-harmony run.bat). NOTE: keep this file ASCII --
REM  cmd decodes batch files as GBK, non-ASCII comments break it.
REM
REM  Requires: private.json in THIS folder (AGC -> Project settings
REM  -> Service account; auto-scanned on start).
REM
REM  Optional hardening (uncomment; see docs/cloudfuctionticketenv.md):
REM    set TICKET_RATE_LIMIT_IP=100
REM    set TICKET_AUTO_BAN=5
REM
REM  Service stops when the window closes; re-run after VPS reboot.
REM  For auto-start/crash-restart use nssm-ticket.bat instead.
REM ============================================================

cd /d "%~dp0"

if not exist "cf-ticket.exe" (
    echo [ERROR] cf-ticket.exe not found in this folder.
    pause
    exit /b
)
if not exist "*.json" (
    echo [ERROR] no .json file in this folder. Put your AGC service account
    echo key here -- ANY filename works, it is auto-scanned by content
    echo ^(must contain a PRIVATE KEY value^).
    pause
    exit /b
)

set PORT=8091
set TICKET_TTL_SECONDS=600
echo Starting Hotify CF-Ticket (anonymous, ttl=600s, port 8091)...
cf-ticket.exe
pause
