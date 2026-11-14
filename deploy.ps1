# disk-hog — one-command deploy for Windows (optional platform).
#
#   powershell -ExecutionPolicy Bypass -File deploy.ps1            build/install/start
#   powershell -ExecutionPolicy Bypass -File deploy.ps1 -Port 8080   (default 6969)
#   powershell -ExecutionPolicy Bypass -File deploy.ps1 -Uninstall
#
# Installs to %LOCALAPPDATA%\disk-hog and registers a per-user scheduled task
# that starts it at logon. No administrator rights needed.
param([int]$Port = 0, [switch]$Uninstall, [switch]$NoPull)
$ErrorActionPreference = 'Stop'
$App = 'disk-hog'
$Here = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $Here
$Dir = Join-Path $env:LOCALAPPDATA $App
$Bin = Join-Path $Dir "$App.exe"
$Task = 'disk-hog'

function Say($m) { Write-Host "==> $m" -ForegroundColor Magenta }

if ($Uninstall) {
    Stop-ScheduledTask -TaskName $Task -ErrorAction SilentlyContinue
    Unregister-ScheduledTask -TaskName $Task -Confirm:$false -ErrorAction SilentlyContinue
    Get-Process $App -ErrorAction SilentlyContinue | Stop-Process -Force
    Remove-Item $Bin -Force -ErrorAction SilentlyContinue
    Say "uninstalled (config kept in $env:APPDATA\$App)"
    exit 0
}

if (-not $NoPull -and (Test-Path .git) -and (Get-Command git -ErrorAction SilentlyContinue) -and (git remote)) {
    Say 'Pulling latest changes'
    git pull --ff-only
}

$src = $null
if ((Test-Path go.mod) -and (Get-Command go -ErrorAction SilentlyContinue)) {
    $version = (git describe --tags --always --dirty 2>$null); if (-not $version) { $version = 'dev' }
    Say "Building $App $version"
    $env:CGO_ENABLED = '0'; $env:GOTOOLCHAIN = 'local'
    New-Item -ItemType Directory -Force build | Out-Null
    go build -trimpath -ldflags "-s -w -X main.version=$version" -o "build\$App.exe" ./cmd/$App
    $src = "build\$App.exe"
} elseif (Test-Path ".\$App.exe") {
    $src = ".\$App.exe"
} else {
    throw "Go is not installed and no prebuilt $App.exe is next to this script."
}

Say "Installing $Bin"
Get-Process $App -ErrorAction SilentlyContinue | Stop-Process -Force
New-Item -ItemType Directory -Force $Dir | Out-Null
Copy-Item $src $Bin -Force
& $Bin config init | Out-Null
if ($Port -gt 0) { & $Bin config set port $Port | Out-Null }

Say 'Registering logon task'
$action = New-ScheduledTaskAction -Execute $Bin -Argument 'serve'
$trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit 0 -Priority 7
Register-ScheduledTask -TaskName $Task -Action $action -Trigger $trigger -Settings $settings -Force | Out-Null
Start-ScheduledTask -TaskName $Task

& $Bin health --wait 10s
