# Deploy a freshly built LingmaProxy.exe and repackage the three site zips,
# asserting by read-back hash at every step. A zip name matching means nothing;
# only the exe inside it does.
#
# Build the exe first with the production tag (this is what `wails build` passes
# for you). Without it Wails links the dev-mode app shell, which never invokes
# OnStartup, so the process sits there alive, logging nothing, binding no port:
#   $env:CGO_ENABLED = '0'
#   go build -tags production -trimpath -ldflags '-s -w -H windowsgui -X main.devtoolsBuild=true' `
#     -o desktop/build/bin/LingmaProxy.exe ./desktop
# The health probe below is the backstop: it fails a build that dropped the tag.
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
  # Replace by rename, never by delete-then-copy. In the gap between the two the
  # folder holds no executable at all, and this box runs the app under a scheduled
  # task that restarts it on failure -- so the restart lands in the gap, the copy
  # then retries against a folder it can never fill, and the deploy ends with
  # nothing installed. Staging beside the target and renaming over it keeps a
  # working binary in place for every instant of the swap.
  $staged = "$to.new"
  for ($i = 1; $i -le 8; $i++) {
    try {
      Copy-Item -Force $from $staged
      $wantStaged = (Get-FileHash -Algorithm SHA256 $from).Hash
      if ((Get-FileHash -Algorithm SHA256 $staged).Hash -ne $wantStaged) {
        throw "staged copy of $from does not match its source"
      }
      Move-Item -Force $staged $to
      return $i
    } catch {
      Start-Sleep -Seconds 2
    }
  }
  throw "could not replace $to"
}

if (-not (Test-Path $TargetDir)) { throw "target dir not found: $TargetDir" }

# Only this folder's instance: cn/intl/both and dev builds share the exe name, and
# stopping a sibling would take down a serving proxy without restarting it.
function Get-Installed {
  @(Get-Process -Name LingmaProxy -ErrorAction SilentlyContinue | Where-Object {
    try { $_.Path -eq $installed } catch { $false }
  })
}

$running = Get-Installed
if ($running) {
  Write-Output ("STOP pid=" + (($running | ForEach-Object { $_.Id }) -join ','))
  $running | Stop-Process -Force
  Start-Sleep -Seconds 3
}

# Keep whatever is being replaced. A rollback target that exists only because
# someone remembered to copy it by hand is not a rollback path.
if (Test-Path $installed) {
  $prev = "$installed.prev"
  Copy-Item -Force $installed $prev
  Write-Output ("PREV sha=" + (Get-FileHash -Algorithm SHA256 $prev).Hash + " -> " + $prev)
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
  # Take the sidecar from the installed folder, not from inside the zip. The one in
  # the zip is whatever shipped when that zip was built; the one in the folder is
  # the tuned copy. Refilling from the zip on every repack silently reverts the hand
  # tuning -- warmup_timeout, the fallback model list, the state-isolating
  # instance_id -- and the revert only shows up as "the GUI is slow again" on the
  # next boot, long after the deploy that caused it.
  $deployedSidecar = Join-Path (Join-Path $TargetDir $variant) 'lingma-proxy.json'
  $sidecarSource = if (Test-Path $deployedSidecar) { $deployedSidecar } else { Join-Path $probe 'lingma-proxy.json' }
  Copy-Item $sidecarSource (Join-Path $stage 'lingma-proxy.json')
  # Build the replacement beside the original and only swap it in once it reads
  # back correctly: a failed Compress-Archive must not cost the variant zip.
  # Compress-Archive insists on a .zip suffix, hence the staging name.
  $fresh = Join-Path $TargetDir "lp-stage-$variant.zip"
  if (Test-Path $fresh) { Remove-Item -Force $fresh }
  Compress-Archive -Path (Join-Path $stage '*') -DestinationPath $fresh -CompressionLevel Optimal
  Expand-Archive -Path $fresh -DestinationPath $probe -Force
  $inside = (Get-FileHash -Algorithm SHA256 (Join-Path $probe $exeName)).Hash
  if ($inside -ne $want) { Remove-Item -Recurse -Force $stage, $probe, $fresh; throw "$variant zip holds $inside, expected $want" }
  $wantSidecar = (Get-FileHash -Algorithm SHA256 $sidecarSource).Hash
  $gotSidecar = (Get-FileHash -Algorithm SHA256 (Join-Path $probe 'lingma-proxy.json')).Hash
  if ($gotSidecar -ne $wantSidecar) { Remove-Item -Recurse -Force $stage, $probe, $fresh; throw "$variant zip sidecar does not match $sidecarSource" }
  Move-Item -Force $fresh $zip
  Remove-Item -Recurse -Force $probe
  Write-Output ("ZIP $variant size=$((Get-Item $zip).Length) exe_sha=$inside sidecar_from=$sidecarSource ok=True")
  Remove-Item -Recurse -Force $stage
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
