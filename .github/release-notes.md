## What's new in v1.1.0

- **Connects on networks that can't reach each other directly.** When the
  friend's page gets no reply to its connectivity checks for 7 s (e.g. a
  carrier-grade NAT facing a strict router), it switches to an encrypted relay
  through the rendezvous. The server only forwards ciphertext. `ooi status`
  shows when the relay is in use, and failed joins are listed with their reason.
- The code is published faster: ICE gathering finishes 1 s after the first
  public candidate (8 s cap), and a stalled server address is abandoned after
  3.5 s.
- `ooi stop` always finishes (a 4 s watchdog backs up the clean shutdown).
- `ooi update` / `ooi install` while ooi is running: the running exe is
  renamed aside, the copy retries for 5 s, and the receiver restarts on the
  new version with the same flags.

Update: run `ooi update`. Your friend just reloads the page.

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
