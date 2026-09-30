# Setup

## Install on a Windows laptop

**Needs:** Windows 11 (or Windows 10 build 19041+), 64-bit. That is all — no
administrator rights, no Go, no compiler, no runtime, no DLLs.

### Option 1 — one line in PowerShell (recommended)

```powershell
irm https://github.com/rahmaniyaShekh/OOI/releases/latest/download/install.ps1 | iex
```

### Option 2 — download and double-click

1. Open <https://github.com/rahmaniyaShekh/OOI/releases/latest>.
2. Download `ooi.exe`.
3. Double-click it. If Windows SmartScreen says "Windows protected your PC",
   click **More info → Run anyway** (the exe is not code-signed).

### Option 3 — from a terminal, with a downloaded exe

```powershell
.\ooi.exe install
```

All three do the same thing:

| | |
|---|---|
| installs to | `%LOCALAPPDATA%\Programs\ooi\ooi.exe` (always this path) |
| PATH | adds that folder to your **user** PATH — open a new terminal afterwards |
| your code | created once in `%LOCALAPPDATA%\ooi\host.code` and printed |

The install path never changes, so PATH stays correct through every update.

---

## Everyday use

```powershell
ooi verify           # once: prove capture protection works on this PC
ooi serve --detach   # start in the background; prints the link
ooi status           # code, link, connection, protection state, video stats
ooi stop             # stop it
```

Send your friend the link it prints (for example
`https://share.mdarif.online/ooi/#QZD-EB4`) or just the code. They open
**share.mdarif.online/ooi** in desktop Chrome, Edge, Brave or Firefox, enter the
code, and pick what to share. They install nothing.

Run `ooi serve` without `--detach` to keep it in the foreground with a live log;
`Ctrl+C` or `Ctrl+Alt+Q` stops it.

### Your code

- It belongs to **this PC** and is **permanent**: it survives restarts,
  `ooi update`, and uninstall/reinstall.
- `ooi code` prints it again at any time.
- `ooi code --new` replaces it; anyone holding the old one can no longer connect.

### The connection service

`share.mdarif.online/ooi` is already running, and every install uses it by
default. **Nobody who installs ooi deploys anything.** It only relays about 1 KB
of encrypted connection setup; the video itself goes directly between the two
computers.

---

## Update and uninstall

```powershell
ooi update             # download the newest release, verify SHA-256, replace in place
ooi update --check     # just report whether one exists
ooi uninstall          # remove the exe and the PATH entry (keeps your code)
ooi uninstall --purge  # also delete your code and state
```

Re-running the one-line installer also updates.

---

## If something does not work

**`ooi` is not recognized.** Open a **new** terminal window — PATH changes only
reach terminals started after the install. Check with `ooi version`.

**It will not connect.** Almost all pairs connect directly. When *both* sides are
behind strict carrier-grade NAT there is no direct path, and a TURN relay is
needed:

```powershell
ooi serve --turn turn:your.turn.host:3478 --turn-user USER --turn-pass PASS
```

TURN only relays the already-encrypted stream; it cannot read it.

**Protection shows OFF in `ooi status`.** This Windows build is older than
19041. Protection is never optional: the overlay refuses to show without it.

---

## For the maintainer only

Nothing below is needed to install or use ooi.

### Build from source

Needs Go 1.26+ and the MSYS2 mingw64 gcc (for the cgo video decoders, whose
static libraries are vendored in `third_party/`):

```powershell
winget install GoLang.Go MSYS2.MSYS2
# in an "MSYS2 MinGW64" shell:  pacman -S mingw-w64-x86_64-gcc
.\build.ps1 -Test
```

### Publish a release

Releases are built by GitHub Actions. Tag and push:

```powershell
git tag v1.1.0
git push origin v1.1.0
```

The workflow builds a static `ooi.exe`, runs the tests, checks that the exe
depends only on built-in Windows DLLs, and uploads `ooi.exe`, `install.ps1` and
`SHA256SUMS.txt` to the release. `ooi update` and the installer pick it up
automatically.

### The connection service (already deployed)

A Cloudflare Worker in `rendezvous/`, mounted at `share.mdarif.online/ooi*` by a
path route that runs before the site's existing Worker; the rest of the domain is
untouched. Redeploy after changing it:

```bash
cd rendezvous && npx wrangler deploy
node test-rendezvous.mjs https://share.mdarif.online/ooi
```
