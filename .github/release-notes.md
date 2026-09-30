## Install (Windows 11, no admin, nothing else needed)

Open **PowerShell** and run:

```powershell
irm https://github.com/rahmaniyaShekh/OOI/releases/latest/download/install.ps1 | iex
```

Or download **ooi.exe** below and double-click it. Either way it installs to
`%LOCALAPPDATA%\Programs\ooi`, adds itself to your PATH, and prints your
permanent code.

## Use it (from any terminal)

```powershell
ooi serve --detach   # start in the background; prints the link for your friend
ooi status           # code, connection, protection state
ooi stop             # stop it
ooi update           # get the newest release
```

Your friend opens the printed link (share.mdarif.online/ooi), picks a window or
screen, and it appears in your capture-protected overlay.

`ooi.exe` is a single static file that depends only on DLLs built into Windows.
Verify the download against `SHA256SUMS.txt`.
