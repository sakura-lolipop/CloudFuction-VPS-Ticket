@echo off
REM ============================================================
REM  Hotify CF 2.0 cf-ticket NSSM installer (Windows service).
REM  Run ONCE on the VPS as ADMINISTRATOR. NOTE: keep ASCII only.
REM
REM  Requires in THIS folder: cf-ticket.exe + private.json (AGC ->
REM  Project settings -> Service account, auto-scanned) + nssm.exe.
REM
REM  Optional: service stops listening on localhost:8091 only; the
REM  existing HotifyTunnel config.yml adds one ingress rule to
REM  expose it publicly (ticket.<domain> -> http://127.0.0.1:8091).
REM
REM  Re-install (after code update):
REM    nssm stop HotifyTicketCF && nssm remove HotifyTicketCF confirm
REM    then run this script again.
REM ============================================================

set NSSM=nssm.exe
cd /d "%~dp0"

if not exist "cf-ticket.exe" (
    echo [ERROR] cf-ticket.exe not found in this folder.
    pause
    exit /b
)
if not exist "private.json" (
    echo [ERROR] private.json not found in this folder ^(AGC - Project settings - Service account^).
    pause
    exit /b
)
if not exist "logs" mkdir logs

echo --- Installing HotifyTicketCF (cf-ticket, anonymous mode) ---
"%NSSM%" install HotifyTicketCF "%cd%\cf-ticket.exe"
"%NSSM%" set HotifyTicketCF AppDirectory "%cd%"
REM defaults = anonymous + TTL 600; tighten here if needed
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
