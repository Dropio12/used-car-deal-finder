<#
.SYNOPSIS
  Crawl the searches in crawler\searches.yml and push the listings to the
  carbuyer Worker. The local alternative to .github\workflows\crawl.yml.

.DESCRIPTION
  - Reads the bearer token from the CARBUYER_INGEST_TOKEN environment variable
    (process, then user scope). It is never passed on a command line.
  - Reads the Worker URL from -ApiUrl or the CARBUYER_API_URL variable.
  - Builds the Go crawler once (crawler\bin\carbuyer.exe) and rebuilds it when
    the Go sources change.
  - Polite: one run at a time (lock file), one request at a time, the delay
    from searches.yml, and it stops at the first error.
  - Appends everything to a daily log in %LOCALAPPDATA%\carbuyer\logs.

.EXAMPLE
  # once, in a normal PowerShell window:
  [Environment]::SetEnvironmentVariable('CARBUYER_API_URL', 'https://carbuyer-api.<you>.workers.dev', 'User')
  # (CARBUYER_INGEST_TOKEN is set the same way; never paste it in a script)
  powershell -NoProfile -ExecutionPolicy Bypass -File scripts\crawl-and-push.ps1

.EXAMPLE
  # every 30 minutes with Windows Task Scheduler (run in PowerShell, as you):
  $script  = (Resolve-Path scripts\crawl-and-push.ps1).Path
  $action  = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument "-NoProfile -NonInteractive -ExecutionPolicy Bypass -File `"$script`""
  $trigger = New-ScheduledTaskTrigger -Once -At (Get-Date) -RepetitionInterval (New-TimeSpan -Minutes 30)
  $settings = New-ScheduledTaskSettingsSet -MultipleInstances IgnoreNew -ExecutionTimeLimit (New-TimeSpan -Minutes 20) -StartWhenAvailable
  Register-ScheduledTask -TaskName 'carbuyer crawl' -Action $action -Trigger $trigger -Settings $settings
  # remove it later: Unregister-ScheduledTask -TaskName 'carbuyer crawl'
#>
[CmdletBinding()]
param(
  [string]$ApiUrl = $env:CARBUYER_API_URL,
  [string]$Searches,
  [string]$Database = (Join-Path $env:LOCALAPPDATA 'carbuyer\carbuyer.db'),
  # Also score locally with the Rust binary (needs scorer\target\release built).
  [switch]$Score,
  # Extra crawler flags, e.g. -ExtraArgs "--offline=testdata/rav4-qc.html" (replay the saved page).
  [string[]]$ExtraArgs = @()
)

$ErrorActionPreference = 'Stop'
$repo = Split-Path -Parent $PSScriptRoot
$crawler = Join-Path $repo 'crawler'
if (-not $Searches) { $Searches = Join-Path $crawler 'searches.yml' }
$dataDir = Join-Path $env:LOCALAPPDATA 'carbuyer'
$logDir = Join-Path $dataDir 'logs'
New-Item -ItemType Directory -Force -Path $logDir | Out-Null
$log = Join-Path $logDir ("crawl-{0:yyyyMMdd}.log" -f (Get-Date))

function Write-Log([string]$msg) {
  $line = "{0:yyyy-MM-dd HH:mm:ss} {1}" -f (Get-Date), $msg
  Add-Content -Path $log -Value $line -Encoding utf8
  Write-Host $line
}

# Token and URL: from the environment only.
if (-not $env:CARBUYER_INGEST_TOKEN) {
  $env:CARBUYER_INGEST_TOKEN = [Environment]::GetEnvironmentVariable('CARBUYER_INGEST_TOKEN', 'User')
}
if (-not $ApiUrl) { $ApiUrl = [Environment]::GetEnvironmentVariable('CARBUYER_API_URL', 'User') }
if (-not $env:CARBUYER_INGEST_TOKEN) { Write-Log 'ERROR: CARBUYER_INGEST_TOKEN is not set (user environment variable).'; exit 2 }
if (-not $ApiUrl) { Write-Log 'ERROR: pass -ApiUrl or set the CARBUYER_API_URL user environment variable.'; exit 2 }
if (-not $ApiUrl.StartsWith('https://')) { Write-Log 'ERROR: the API URL must start with https://'; exit 2 }

# One run at a time, even if Task Scheduler and a manual run overlap.
$lockPath = Join-Path $dataDir 'crawl.lock'
try {
  $lock = [System.IO.File]::Open($lockPath, 'OpenOrCreate', 'ReadWrite', 'None')
} catch {
  Write-Log 'another crawl is still running; skipping this one'
  exit 0
}

try {
  # Go: on PATH, or the default install folder.
  $go = (Get-Command go -ErrorAction SilentlyContinue).Source
  if (-not $go) { $go = Join-Path $env:ProgramFiles 'Go\bin\go.exe' }
  if (-not (Test-Path $go)) { Write-Log "ERROR: Go not found (looked on PATH and at $go)"; exit 2 }

  # Build when missing or older than any Go source / go.mod.
  $exe = Join-Path $crawler 'bin\carbuyer.exe'
  $newest = Get-ChildItem -Path $crawler -Recurse -Include *.go, go.mod, go.sum -File |
    Where-Object { $_.FullName -notlike '*\bin\*' } |
    Sort-Object LastWriteTime -Descending | Select-Object -First 1
  if (-not (Test-Path $exe) -or ((Get-Item $exe).LastWriteTime -lt $newest.LastWriteTime)) {
    Write-Log 'building the crawler'
    Push-Location $crawler
    try {
      & $go build -trimpath -o $exe ./cmd/carbuyer
      if ($LASTEXITCODE -ne 0) { Write-Log "ERROR: go build failed ($LASTEXITCODE)"; exit 1 }
    } finally { Pop-Location }
  }

  New-Item -ItemType Directory -Force -Path (Split-Path -Parent $Database) | Out-Null
  $cliArgs = @('--searches', $Searches, '--db', $Database, '--push', $ApiUrl)
  if (-not $Score) { $cliArgs += '--no-score' }
  $cliArgs += $ExtraArgs

  Write-Log "crawl start: $Searches -> $ApiUrl"
  Push-Location $crawler
  try {
    # The CLI prints progress on stderr; keep both streams in the log.
    # (cmd /c avoids PowerShell 5.1 turning stderr lines into error records.)
    $quoted = ($cliArgs | ForEach-Object { '"' + $_ + '"' }) -join ' '
    $output = & cmd.exe /c "`"$exe`" $quoted 2>&1"
    $code = $LASTEXITCODE
  } finally { Pop-Location }
  foreach ($line in $output) { Write-Log "  $line" }
  Write-Log "crawl end: exit $code"
  exit $code
} finally {
  $lock.Close()
  Remove-Item -Path $lockPath -ErrorAction SilentlyContinue
}
