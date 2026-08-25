# pack.ps1 -- one-shot VPS deploy kit zipper (keep this file ASCII-only:
# PS 5.1 reads no-BOM UTF-8 as ANSI, non-ASCII comments corrupt parsing).
# Rule: kit files are ALWAYS copied fresh from the repo; staging dir is
# rebuilt from scratch every run (lesson 2026-08-25: stale bat in staging
# shipped an 8091-era file while the repo said 12346).
# Usage: pwsh pack.ps1 [-Out <zip path>] (default: Desktop\hotify-ticket.zip)
param([string]$Out = "$env:USERPROFILE\Desktop\hotify-ticket.zip")
$ErrorActionPreference = "Stop"
$repo = $PSScriptRoot
$stage = Join-Path $env:TEMP "hotify-ticket-pack"

if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
New-Item -ItemType Directory $stage | Out-Null

go build -o (Join-Path $stage "cf-ticket.exe") .
if ($LASTEXITCODE -ne 0) { throw "go build failed" }

Copy-Item (Join-Path $repo "run-ticket.bat") $stage
Copy-Item (Join-Path $repo "nssm-ticket.bat") $stage
Copy-Item (Join-Path $repo "DEPLOY.txt") (Join-Path $stage "README.txt")

# nssm.exe borrowed from local go-harmony folder (adjust path if needed)
$nssm = "C:\Users\littl\bark\CloudFuction-vps\harmony\nssm.exe"
if (Test-Path $nssm) { Copy-Item $nssm $stage } else { Write-Warning "nssm.exe not found at $nssm (kit ships without it)" }

if (Test-Path $Out) { Remove-Item -Force $Out }
Compress-Archive -Path (Join-Path $stage "*") -DestinationPath $Out
Write-Host "packed -> $Out"
Get-ChildItem $stage | Format-Table Name, Length -AutoSize
