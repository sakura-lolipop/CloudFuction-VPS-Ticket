# pack.ps1 —— 一键打 VPS 部署套件 zip（2026-08-25 固化：zip 内容永远从仓现拷，
# 禁止手攒组装目录——教训：组装目录残留旧 bat，zip 与仓不一致，用户拖到 8091 旧件）。
# 用法：pwsh pack.ps1 [-Out <zip 路径>]（默认桌面 hotify-ticket.zip）
param([string]$Out = "$env:USERPROFILE\Desktop\hotify-ticket.zip")
$ErrorActionPreference = "Stop"
$repo = $PSScriptRoot
$stage = Join-Path $env:TEMP "hotify-ticket-pack"

if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }   # 每次全新（核心：绝不复用）
New-Item -ItemType Directory $stage | Out-Null

go build -o (Join-Path $stage "cf-ticket.exe") .
if ($LASTEXITCODE -ne 0) { throw "go build failed" }

Copy-Item (Join-Path $repo "run-ticket.bat") $stage
Copy-Item (Join-Path $repo "nssm-ticket.bat") $stage
Copy-Item (Join-Path $repo "DEPLOY.txt") (Join-Path $stage "README.txt")

# nssm.exe：本机 go-harmony 目录借（联邦用户自备或改路径）
$nssm = "C:\Users\littl\bark\CloudFuction-vps\harmony\nssm.exe"
if (Test-Path $nssm) { Copy-Item $nssm $stage } else { Write-Warning "nssm.exe not found at $nssm (kit ships without it)" }

if (Test-Path $Out) { Remove-Item -Force $Out }
Compress-Archive -Path (Join-Path $stage "*") -DestinationPath $Out
Write-Host "packed -> $Out"
Get-ChildItem $stage | Format-Table Name, Length -AutoSize
