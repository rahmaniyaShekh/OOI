## Install (Windows 11, no admin, nothing else needed)

Download **ooi.exe** below and double-click it. It installs to
`%LOCALAPPDATA%\Programs\ooi`, adds itself to your PATH, and prints your
permanent code.

With a read-only GitHub token you can instead run, in PowerShell:

```powershell
$env:OOI_GITHUB_TOKEN = 'github_pat_...'
irm https://api.github.com/repos/rahmaniyaShekh/OOI/contents/install.ps1 -Headers @{Authorization="Bearer $env:OOI_GITHUB_TOKEN"; Accept='application/vnd.github.raw'} | iex
```

## Use (from any terminal)

```powershell
ooi start     # start in the background; prints the link for your friend
ooi status    # code, connection, protection state
ooi stop      # stop it
ooi update    # get the newest release
```

Your friend opens the printed link (share.mdarif.online/ooi), picks a window or
screen, and it appears in your capture-protected overlay.

`ooi.exe` is a single static file that depends only on DLLs built into Windows.
Verify it against `SHA256SUMS.txt`.
