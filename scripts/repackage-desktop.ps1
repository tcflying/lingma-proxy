# Deploy a freshly built LingmaProxy.exe and repackage the three site zips,
# asserting by read-back hash at every step. A zip name matching means nothing;
# only the exe inside it does.
param(
  [Parameter(Mandatory = $true)][string]$Exe,
  [string]$TargetDir = "$env:USERPROFILE\Desktop\qoder fan",
  [switch]$SkipRun
)

$ErrorActionPreference = 'Stop'
$src = (Resolve-Path $Exe).Path
$want = (Get-FileHash -Algorithm SHA256 $src).Hash
$exeName = 'LingmaProxy.exe'
$installed = Join-Path $TargetDir $exeName

function Swap-In($from, $to) {
  # A process that was just killed can still hold the target open, so the
  # delete-then-copy has to retry rather than assume the first attempt works.
  for ($i = 1; $i -le 8; $i++) {
    try {
      if (Test-Path $to) { Remove-Item -Force $to }
      Copy-Item -Force $from $to
      return $i
    } catch {
      Start-Sleep -Seconds 2
    }
  }
  throw "could not replace $to"
}

if (-not (Test-Path $TargetDir)) { throw "target dir not found: $TargetDir" }

$running = Get-Process -Name LingmaProxy -ErrorAction SilentlyContinue
if ($running) {
  Write-Output ("STOP pid=" + (($running | ForEach-Object { $_.Id }) -join ','))
  $running | Stop-Process -Force
  Start-Sleep -Seconds 3
}

$attempt = Swap-In $src $installed
$got = (Get-FileHash -Algorithm SHA256 $installed).Hash
if ($got -ne $want) { throw "installed hash mismatch: $got != $want" }
Write-Output ("INSTALLED sha=$got bytes=$((Get-Item $installed).Length) attempt=$attempt")

# Repack each variant from the build just installed, keeping that variant's own
# sidecar config, then read the archive back to prove what actually went in.
foreach ($variant in @('both', 'cn', 'intl')) {
  $zip = Join-Path $TargetDir "LingmaProxy-$variant-win-x64.zip"
  if (-not (Test-Path $zip)) { throw "missing variant zip: $zip" }
  $stage = Join-Path $env:TEMP "lpk-$variant"
  $probe = Join-Path $env:TEMP "lpu-$variant"
  foreach ($d in @($stage, $probe)) { if (Test-Path $d) { Remove-Item -Recurse -Force $d } }
  New-Item -ItemType Directory -Force $stage | Out-Null
  Expand-Archive -Path $zip -DestinationPath $probe -Force
  Copy-Item $installed (Join-Path $stage $exeName)
  Copy-Item (Join-Path $probe 'lingma-proxy.json') (Join-Path $stage 'lingma-proxy.json')
  Remove-Item -Force $zip
  Compress-Archive -Path (Join-Path $stage '*') -DestinationPath $zip -CompressionLevel Optimal
  Remove-Item -Recurse -Force $probe
  Expand-Archive -Path $zip -DestinationPath $probe -Force
  $inside = (Get-FileHash -Algorithm SHA256 (Join-Path $probe $exeName)).Hash
  if ($inside -ne $want) { throw "$variant zip holds $inside, expected $want" }
  Write-Output ("ZIP $variant size=$((Get-Item $zip).Length) exe_sha=$inside ok=True")
  Remove-Item -Recurse -Force $stage, $probe
}

if ($SkipRun) { Write-Output 'RUN skipped'; exit 0 }

Start-Process -FilePath $installed -WorkingDirectory $TargetDir
Start-Sleep -Seconds 12
$started = Get-Process -Name LingmaProxy -ErrorAction SilentlyContinue
if (-not $started) { throw 'the app did not stay running' }
Write-Output ("RUNNING pid=" + (($started | ForEach-Object { $_.Id }) -join ','))

# The sidecar shipped inside the variant zip says which port this folder serves,
# so the check cannot drift onto another instance that happens to be running.
$probe = Join-Path $env:TEMP 'lpq-probe'
if (Test-Path $probe) { Remove-Item -Recurse -Force $probe }
New-Item -ItemType Directory -Force $probe | Out-Null
Expand-Archive -Path (Join-Path $TargetDir 'LingmaProxy-both-win-x64.zip') -DestinationPath $probe -Force
$port = [int]((Get-Content (Join-Path $probe 'lingma-proxy.json') -Raw | ConvertFrom-Json).port)
Remove-Item -Recurse -Force $probe

$deadline = (Get-Date).AddSeconds(150)
$status = 0
while ((Get-Date) -lt $deadline) {
  try {
    $status = (Invoke-WebRequest -Uri "http://127.0.0.1:$port/health" -UseBasicParsing -TimeoutSec 15).StatusCode
    if ($status -eq 200) { break }
  } catch { Start-Sleep -Seconds 3 }
}
Write-Output "PROXY port=$port health=$status"
$admin = try {
  (Invoke-WebRequest -Uri "http://127.0.0.1:$($port + 1)/api/admin/status" -UseBasicParsing -TimeoutSec 5).StatusCode
  'OPEN-WITHOUT-TOKEN'
} catch { $_.Exception.Response.StatusCode.value__ }
Write-Output "CONSOLE port=$($port + 1) noauth=$admin"
if ($status -ne 200) { throw "proxy health=$status" }
if ($admin -ne 401) { throw "console answered without a token ($admin)" }
Write-Output 'DONE'
