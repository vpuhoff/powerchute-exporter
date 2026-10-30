param([Parameter(Mandatory)][string]$Installer)
$ErrorActionPreference = 'Stop'
if ($env:GITHUB_ACTIONS -ne 'true') { throw 'This installation smoke test may run only on a disposable GitHub Actions runner' }
$Installer = (Resolve-Path -LiteralPath $Installer).Path
$app = Join-Path $env:ProgramFiles 'PowerChute Exporter Test'
$data = Join-Path $env:ProgramData 'PowerChute Exporter'
$config = Join-Path $data 'config.yaml'

function Install-Exporter {
    $process = Start-Process -FilePath $Installer -ArgumentList @('/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART', '/SP-', "/DIR=`"$app`"") -WindowStyle Hidden -Wait -PassThru
    if ($process.ExitCode -ne 0) { throw "Installer failed: $($process.ExitCode)" }
}

function Wait-Exporter([string]$Status, [int]$Seconds) {
    $service = Get-Service PowerChuteExporter
    try { $service.WaitForStatus($Status, [TimeSpan]::FromSeconds($Seconds)) }
    finally { $service.Dispose() }
}

try {
    Install-Exporter
    $service = Get-Service PowerChuteExporter
    try { if ($service.Status -ne 'Stopped') { throw 'Unconfigured installation must stay stopped' } }
    finally { $service.Dispose() }
    $details = Get-CimInstance Win32_Service -Filter "Name='PowerChuteExporter'"
    if ($details.StartName -ne 'NT AUTHORITY\LocalService') { throw 'Unexpected service identity' }
    if ($details.StartMode -ne 'Auto') { throw 'Service is not enabled for automatic startup' }

    @'
powerchute:
  base_url: http://127.0.0.1:1
  timezone: UTC
  username: installer-test-user
  password: installer-test-password
energy:
  currency: USD
datalog:
  enabled: false
'@ | Set-Content -LiteralPath $config -Encoding utf8
    $expected = (Get-FileHash -LiteralPath $config).Hash
    Start-Service PowerChuteExporter
    Wait-Exporter 'Running' 30
    $metrics = Invoke-WebRequest -UseBasicParsing http://127.0.0.1:9787/metrics
    if ($metrics.Content -notmatch 'powerchute_exporter_build_info') { throw 'Service does not expose metrics' }

    # Updating a running service must stop it, preserve the config and restart it.
    Install-Exporter
    if ((Get-FileHash -LiteralPath $config).Hash -ne $expected) { throw 'Upgrade overwrote the configuration' }
    Wait-Exporter 'Running' 30
    Stop-Service PowerChuteExporter
    Wait-Exporter 'Stopped' 35
} finally {
    $uninstaller = Join-Path $app 'unins000.exe'
    if (Test-Path -LiteralPath $uninstaller) {
        [GC]::Collect()
        [GC]::WaitForPendingFinalizers()
        $process = Start-Process -FilePath $uninstaller -ArgumentList @('/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART') -WindowStyle Hidden -Wait -PassThru
        if ($process.ExitCode -ne 0) { throw "Uninstaller failed: $($process.ExitCode)" }
        $remaining = Get-Service PowerChuteExporter -ErrorAction SilentlyContinue
        if ($remaining) { $remaining.Dispose(); throw 'Uninstaller left the service registered' }
        if (-not (Test-Path -LiteralPath $config)) { throw 'Uninstaller deleted the user configuration' }
    }
}
