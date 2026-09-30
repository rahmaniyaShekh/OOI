# OOI installer - one line, no admin, no prerequisites:
#
#   irm https://github.com/rahmaniyaShekh/OOI/releases/latest/download/install.ps1 | iex
#
# Downloads the prebuilt ooi.exe from the latest GitHub release, verifies its
# SHA-256, installs it to %LOCALAPPDATA%\Programs\ooi and adds that folder to
# your user PATH. Nothing else is needed: no Go, no compiler, no runtime.
# Re-running it updates to the latest release. Your join code is kept.

$ErrorActionPreference = 'Stop'
$ProgressPreference    = 'SilentlyContinue'   # Invoke-WebRequest is 10x faster without the progress bar
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$repo = 'rahmaniyaShekh/OOI'
$base = "https://github.com/$repo/releases/latest/download"

if ([Environment]::OSVersion.Version.Build -lt 19041) {
    throw "OOI needs Windows 10 build 19041+ or Windows 11 (this is build $([Environment]::OSVersion.Version.Build))."
}
if (-not [Environment]::Is64BitOperatingSystem) {
    throw 'OOI needs 64-bit Windows.'
}

$tmp = Join-Path ([IO.Path]::GetTempPath()) ("ooi-install-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Write-Host 'Downloading ooi.exe from GitHub...'
    $exe  = Join-Path $tmp 'ooi.exe'
    $sums = Join-Path $tmp 'SHA256SUMS.txt'
    Invoke-WebRequest -UseBasicParsing -Uri "$base/ooi.exe"        -OutFile $exe
    Invoke-WebRequest -UseBasicParsing -Uri "$base/SHA256SUMS.txt" -OutFile $sums

    # Refuse anything that does not match the published checksum.
    $want = (Get-Content $sums | Where-Object { $_ -match '\s\*?ooi\.exe$' } | Select-Object -First 1) -split '\s+' | Select-Object -First 1
    $got  = (Get-FileHash -Algorithm SHA256 $exe).Hash
    if (-not $want -or $got -ne $want.ToUpper()) {
        throw "Checksum mismatch for ooi.exe (expected $want, got $got). Nothing was installed."
    }
    Write-Host 'Checksum verified.'

    # Stop a running copy so the new one takes over.
    $installed = Join-Path $env:LOCALAPPDATA 'Programs\ooi\ooi.exe'
    if (Test-Path $installed) { & $installed stop 2>$null | Out-Null }

    & $exe install
    if ($LASTEXITCODE -ne 0) { throw "ooi install failed (exit $LASTEXITCODE)." }

    # Make `ooi` usable in THIS window too, not only in new ones.
    $dir = Split-Path $installed
    if (-not (($env:Path -split ';') -contains $dir)) { $env:Path = "$env:Path;$dir" }
    Write-Host ''
    Write-Host 'Done. Try:  ooi serve --detach' -ForegroundColor Green
}
finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
