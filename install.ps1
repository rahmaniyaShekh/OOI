# OOI installer. No admin, nothing else to install.
#
# The OOI repository is private, so GitHub needs a read-only token to hand out
# the release. Create one once (per person, not per machine) at
#   https://github.com/settings/personal-access-tokens/new
#   Repository access: Only select repositories -> OOI
#   Permissions:       Contents -> Read-only
#
# Then, in PowerShell:
#
#   $env:OOI_GITHUB_TOKEN = 'github_pat_...'
#   irm https://api.github.com/repos/rahmaniyaShekh/OOI/contents/install.ps1 -Headers @{Authorization="Bearer $env:OOI_GITHUB_TOKEN"; Accept='application/vnd.github.raw'} | iex
#
# (or run this file directly: it asks for the token if none is set).
#
# It downloads the latest ooi.exe from the release, verifies its SHA-256,
# installs it to %LOCALAPPDATA%\Programs\ooi, adds that to your user PATH, and
# saves the token encrypted for your Windows account so `ooi update` just works.
# Re-running it updates. Your join code is kept.

$ErrorActionPreference = 'Stop'
$repo = 'rahmaniyaShekh/OOI'

if ([Environment]::OSVersion.Version.Build -lt 19041) {
    throw "OOI needs Windows 10 build 19041+ or Windows 11 (this is build $([Environment]::OSVersion.Version.Build))."
}
if (-not [Environment]::Is64BitOperatingSystem) { throw 'OOI needs 64-bit Windows.' }
$curl = Join-Path $env:SystemRoot 'System32\curl.exe'   # built into Windows 10 1803+ / 11
if (-not (Test-Path $curl)) { throw 'curl.exe not found (Windows 10 1803+ / Windows 11 include it).' }

$token = $env:OOI_GITHUB_TOKEN
if (-not $token) { $token = $env:GITHUB_TOKEN }
if (-not $token) {
    Write-Host 'The OOI repository is private. Paste a GitHub token with read access to it.'
    Write-Host 'Create one at https://github.com/settings/personal-access-tokens/new'
    Write-Host '(Only select repositories: OOI; Permissions: Contents = Read-only)'
    $sec = Read-Host -AsSecureString 'Token'
    $token = [Runtime.InteropServices.Marshal]::PtrToStringBSTR([Runtime.InteropServices.Marshal]::SecureStringToBSTR($sec))
}
$token = $token.Trim()
if (-not $token) { throw 'No token given; nothing was installed.' }

[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
$headers = @{ Authorization = "Bearer $token"; Accept = 'application/vnd.github+json'; 'X-GitHub-Api-Version' = '2022-11-28' }
try {
    $rel = Invoke-RestMethod -UseBasicParsing -Headers $headers -Uri "https://api.github.com/repos/$repo/releases/latest"
} catch {
    throw "GitHub refused the request. Check that the token is valid and has read access to $repo. ($($_.Exception.Message))"
}
$exeAsset = $rel.assets | Where-Object name -eq 'ooi.exe'        | Select-Object -First 1
$sumAsset = $rel.assets | Where-Object name -eq 'SHA256SUMS.txt' | Select-Object -First 1
if (-not $exeAsset -or -not $sumAsset) { throw "Release $($rel.tag_name) is missing ooi.exe or SHA256SUMS.txt." }
Write-Host "Latest release: $($rel.tag_name)"

$tmp = Join-Path ([IO.Path]::GetTempPath()) ("ooi-install-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    $exe  = Join-Path $tmp 'ooi.exe'
    $sums = Join-Path $tmp 'SHA256SUMS.txt'
    foreach ($pair in @(@($exeAsset.url, $exe), @($sumAsset.url, $sums))) {
        # curl drops the Authorization header when GitHub redirects to its
        # storage host, which is exactly what that signed URL requires.
        & $curl -fsSL -H "Authorization: Bearer $token" -H 'Accept: application/octet-stream' -o $pair[1] $pair[0]
        if ($LASTEXITCODE -ne 0) { throw "Download failed (curl exit $LASTEXITCODE)." }
    }

    $want = ((Get-Content $sums | Where-Object { $_ -match '\s\*?ooi\.exe$' } | Select-Object -First 1) -split '\s+')[0]
    $got  = (Get-FileHash -Algorithm SHA256 $exe).Hash
    if (-not $want -or $got -ne $want.ToUpper()) {
        throw "Checksum mismatch for ooi.exe (expected $want, got $got). Nothing was installed."
    }
    Write-Host 'Downloaded and verified (SHA-256).'

    $installed = Join-Path $env:LOCALAPPDATA 'Programs\ooi\ooi.exe'
    if (Test-Path $installed) { & $installed stop 2>$null | Out-Null }

    # Hand the token over through the environment (not the command line) so
    # `ooi install` saves it encrypted for later updates.
    $env:OOI_GITHUB_TOKEN = $token
    & $exe install
    if ($LASTEXITCODE -ne 0) { throw "ooi install failed (exit $LASTEXITCODE)." }

    $dir = Split-Path $installed
    if (-not (($env:Path -split ';') -contains $dir)) { $env:Path = "$env:Path;$dir" }
    Write-Host ''
    Write-Host 'Done. In this window or any new one:  ooi start' -ForegroundColor Green
}
finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    Remove-Item Env:\OOI_GITHUB_TOKEN -ErrorAction SilentlyContinue
}
