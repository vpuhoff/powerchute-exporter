param([Parameter(Mandatory)][string]$ToolDirectory)
$ErrorActionPreference = 'Stop'
$target = [IO.Path]::GetFullPath($ToolDirectory)
New-Item -ItemType Directory -Force -Path $target | Out-Null
$installer = Join-Path $target 'innosetup-6.7.4.exe'
Invoke-WebRequest -Uri 'https://github.com/jrsoftware/issrc/releases/download/is-6_7_4/innosetup-6.7.4.exe' -OutFile $installer -TimeoutSec 120
$expected = 'a549929319f9120716677c685535ca4671e4b937b5fdc87e2585f32d8eb30e8c'
if ((Get-FileHash -Algorithm SHA256 -LiteralPath $installer).Hash.ToLowerInvariant() -ne $expected) {
    throw 'Inno Setup download checksum mismatch'
}
$process = Start-Process -FilePath $installer -ArgumentList @(
    '/VERYSILENT', '/SUPPRESSMSGBOXES', '/SP-', '/NORESTART', '/NOICONS', '/PORTABLE=1', "/DIR=`"$target`""
) -WindowStyle Hidden -Wait -PassThru
if ($process.ExitCode -ne 0) { throw "Inno Setup compiler setup failed: $($process.ExitCode)" }
$compiler = Join-Path $target 'ISCC.exe'
if (-not (Test-Path -LiteralPath $compiler)) { throw 'ISCC.exe was not installed' }
if ($env:GITHUB_OUTPUT) { "iscc=$compiler" | Out-File -Append -FilePath $env:GITHUB_OUTPUT -Encoding utf8 }
Write-Output $compiler
