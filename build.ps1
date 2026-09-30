# build.ps1 - build ooi.exe on Windows.
#
# ooi links three video decoders (libvpx, dav1d, openh264) through cgo, so it
# needs a C compiler. This script finds the MSYS2 mingw64 gcc, sets the cgo
# environment, and produces a single static ooi.exe that depends only on
# Windows' own DLLs (KERNEL32, msvcrt) - copy it to any Windows 11 laptop and
# run it, no toolchain or runtime required.
#
#   .\build.ps1              # build ooi.exe
#   .\build.ps1 -Test        # build, then run the offline test suite
#   .\build.ps1 -Version 1.2.0

param(
    [string]$Version = "dev",
    [switch]$Test
)

$ErrorActionPreference = "Stop"

# Locate a mingw64 gcc. Adjust MSYS2 path if yours differs.
$gccDirs = @("C:\msys64\mingw64\bin", "C:\mingw64\bin")
$gcc = $null
foreach ($d in $gccDirs) {
    if (Test-Path (Join-Path $d "gcc.exe")) { $gcc = $d; break }
}
if (-not $gcc) {
    if (Get-Command gcc -ErrorAction SilentlyContinue) {
        $gcc = Split-Path (Get-Command gcc).Source
    } else {
        Write-Error "No gcc found. Install MSYS2 (https://www.msys2.org), then:  pacman -S mingw-w64-x86_64-gcc"
    }
}
Write-Host "Using gcc from $gcc"
$env:PATH = "$gcc;$env:PATH"
$env:CGO_ENABLED = "1"
$env:CC = "gcc"
$env:CXX = "g++"

$ldflags = "-s -w -X main.version=$Version"
Write-Host "Building ooi.exe (version $Version)..."
go build -ldflags $ldflags -o ooi.exe .
if ($LASTEXITCODE -ne 0) { Write-Error "build failed" }

Write-Host "Built ooi.exe" -ForegroundColor Green
$deps = & objdump -p ooi.exe 2>$null | Select-String "DLL Name" | ForEach-Object { $_.Line.Trim() }
if ($deps) { Write-Host "DLL dependencies:"; $deps | ForEach-Object { Write-Host "  $_" } }

if ($Test) {
    Write-Host "Running offline tests..."
    go test -short ./internal/...
    if ($LASTEXITCODE -ne 0) { Write-Error "tests failed" }
    Write-Host "Tests passed" -ForegroundColor Green
}
