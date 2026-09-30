# ooi

Watch a friend's screen — shared from their browser, anywhere on the internet —
in an overlay on your Windows PC that **Windows excludes from every software
screen-capture path**. The overlay is visible to your eyes but absent from OBS,
Xbox Game Bar, the Snipping Tool, Teams, Zoom, Discord, Print Screen, and
anything else that records the desktop through Windows' own APIs.

## Install

**Windows 11** (or Windows 10 build 19041+). No admin rights, and **nothing else
to install**: no Go, no compiler, no runtime. The release is one prebuilt,
self-contained `ooi.exe`. The repository is private, so downloading needs your
GitHub access.

**Easiest: browser + double-click**

1. Sign in to GitHub and open the latest
   [release](https://github.com/rahmaniyaShekh/OOI/releases/latest).
2. Download `ooi.exe` and **double-click it**.

It installs itself to `%LOCALAPPDATA%\Programs\ooi`, adds that folder to your
user PATH, and prints **your code**.

**Or from PowerShell** (with a read-only GitHub token, see [SETUP.md](SETUP.md#get-a-github-token)):

```powershell
$env:OOI_GITHUB_TOKEN = 'github_pat_...'
irm https://api.github.com/repos/rahmaniyaShekh/OOI/contents/install.ps1 -Headers @{Authorization="Bearer $env:OOI_GITHUB_TOKEN"; Accept='application/vnd.github.raw'} | iex
```

This also saves the token encrypted for your Windows account, so `ooi update`
needs nothing more.

## Use

Everything runs from a terminal:

```powershell
ooi start            # start in the background (the default); prints the link
ooi status           # your code, connection, protection state, video stats
ooi stop             # stop it
ooi verify           # prove capture protection works on this PC
ooi update           # install the newest release (SHA-256 verified)
ooi uninstall        # remove it (keeps your code; --purge deletes it)
```

`ooi start --foreground` keeps it in the terminal with a live log. In the
background it logs to `%LOCALAPPDATA%\ooi\ooi.log`.

Send your friend the printed link, e.g. `https://share.mdarif.online/ooi/#QZD-EB4`.
They open it in Chrome/Edge/Firefox, click **Choose what to share**, and your
overlay shows their screen. They install nothing.

**Your code is permanent for this PC.** It is created once and kept across
restarts, updates and reinstalls, so your friend can reconnect any day with the
same link. `ooi code --new` issues a fresh one (the old one stops working).

The connection service at `share.mdarif.online/ooi` is already running and is
shared by every install — there is nothing to deploy.

---

## How the connection works

One side (you) runs `ooi start`. It shows a persistent 6-character code such as
`K7Q-4MX`. Your friend opens **https://share.mdarif.online/ooi**, types the code
(or opens the link, which fills it in), picks a window or screen to share, and
within a second or two their screen is in your overlay — flowing **directly
between the two machines**, encrypted end to end.

```
 YOU (ooi.exe, the host)          RENDEZVOUS (Cloudflare Worker)      FRIEND (browser)
 ─────────────────────            ──────────────────────────         ────────────────
 code = K7Q4MX (persistent)
 make WebRTC offer, seal it
 POST /ooi/api/room  ───────────►  stores the sealed offer
 poll for answer  ◄────────────►                        ◄─────────── types K7Q-4MX
                                   hands over the offer  ───────────► getDisplayMedia()
                                   stores sealed answer  ◄─────────── seals answer
 read answer  ◄─────────────────  (read once, deleted)
 ════════ WebRTC: video flows directly, peer to peer; the server is gone ════════
 decode (VP9/AV1/VP8/H.264) → draw into the capture-excluded overlay
```

* The **code is both the address and the key**: `SHA-256(code)` says *where* the
  offer is stored; a key derived from the code (PBKDF2) *decrypts* it. The code
  itself never reaches the server, and the server never sees your IP or SDP in
  plaintext.
* The **code belongs to your device**. It survives restarts, so your friend
  rejoins tomorrow with nothing re-sent. Rotate it any time with
  `ooi start --new-code` (or `ooi code --new`) to revoke old access.
* The only server is a tiny Cloudflare Worker that swaps ~1 KB of encrypted
  connection data and then leaves the path. The full pattern is in
  [P2P_SHORT_CODE_CONNECT.md](P2P_SHORT_CODE_CONNECT.md).

---

## Video quality on a slow connection

The whole pipeline is built to hold a **sharp, readable picture on a weak link**
and to sharpen automatically when the link improves.

* **Low frame rate on purpose.** The default is **8 fps** (choosable 5–15 on the
  share page). Spending the bandwidth budget on fewer, higher-quality frames
  keeps text legible, which is what matters for a shared screen.
* **Resolution is preserved over smoothness.** The browser encoder is set to
  `degradation-preference: maintain-resolution` with the screen-content hint, so
  when bandwidth tightens it drops frames rather than blurring the image.
* **It ramps up on its own.** A high bitrate ceiling lets WebRTC's congestion
  control (TWCC/GCC) use more of the pipe as it clears, and back off before a
  slow link starts dropping — so quality rises on fast connections and holds
  together on slow ones, with no manual switch.
* **Loss is repaired, not smeared.** Lost packets are recovered by
  retransmission (NACK/RTX). A custom jitter buffer waits about one round trip
  for the repair before giving up, and only then asks for a fresh keyframe —
  which is expensive on a slow link. Frames that would decode against a missing
  reference are **never shown**; the picture freezes on the last good frame
  instead of tearing.
* **Modern codecs.** VP9 is preferred (excellent at low bitrates, verified end to
  end here), then AV1, with VP8/H.264 as universal fallbacks. Decoding is native
  (libvpx, dav1d, openh264) with a high-quality antialiased resampler, so the
  overlay stays crisp at any size.
* **Reconnects are automatic.** Wi-Fi blips, a laptop sleeping, the host
  restarting — the two sides re-converge on the same code with exponential
  backoff, and the friend's page waits and rejoins on its own.

---

## The capture protection

One documented Windows API does the work:

```c
SetWindowDisplayAffinity(hwnd, WDA_EXCLUDEFROMCAPTURE);  // 0x11
```

DWM composites the window to your physical display but omits it from the surface
every software capture API reads — the same mechanism DRM video players and
password managers use. In `serve` it is **always on**; there is no flag or
hotkey to turn it off.

The protection is designed to hold **in every situation**, and there are tests
that prove it:

* Applied **before the window is ever shown**, so it is never visible unprotected
  for even one frame.
* A watchdog re-checks it every 2 seconds and re-applies it if anything clears
  it. `ooi status` reports the state read back from the OS, not a local flag.
* **Fail-closed:** before any frame is pushed to the screen, and before the
  overlay is ever re-shown, the exclusion is confirmed with the OS. If it cannot
  be confirmed, the overlay takes itself off screen rather than show one
  unprotected frame.
* The window handle is **never destroyed or recreated** — not on resize, move,
  opacity change, hide/show, or reconnect — because the exclusion is a property
  of that handle. Every operation re-blends the same handle.
* When you disable the overlay (see gestures), it is **cloaked and its surface
  wiped** while the stream keeps running in the background, so nothing leaks
  while it is "off" either.

Verified on this machine with `ooi verify` — every method **100% → 0.0%**:

| Capture method | Unprotected | Protected |
|---|---|---|
| WGC monitor capture (Game Bar, Snipping Tool, Teams, OBS) | seen | **absent** |
| WGC window capture ("share a window") | seen | **absent** |
| DXGI Desktop Duplication (OBS, Parsec, remote desktop) | seen | **absent** |
| GDI BitBlt (+/- CAPTUREBLT), StretchBlt, desktop DC | seen | **absent** |
| PrintWindow (full content) | seen | **absent** |

The automated suite additionally proves the overlay stays uncapturable **while a
live stream is running** and **through every mode** — join, rejoin, repair
(reconnect), enable, disable, moving, and transparency changes.

### What it does not defeat

A compositor-level exclusion, so anything reading pixels *after* the GPU or
*below* the OS still sees the window: a phone or camera pointed at the screen, an
HDMI capture card, or kernel-mode/mirror capture drivers. For a second-screen
use case it is completely effective; it is not an anti-forensics tool.

---

## Controls

### Hotkeys

| Keys | Action |
|---|---|
| `Ctrl+Alt+H` | hide / show the overlay (off screen; **streaming continues**) |
| `Ctrl+Alt+Q` | quit |
| `Ctrl+Alt+M` | toggle mouse click-through |
| `Ctrl+Alt+←↑→↓` | move the overlay |
| `Ctrl+Alt+Shift+←↑→↓` | resize the overlay |
| `Ctrl+Alt+ + / −` | opacity up / down |

There is deliberately **no key that turns protection off.**

### Touchpad gestures

The touchpad is watched **passively and in parallel** — nothing is hooked,
delayed, or swallowed, so ordinary cursor movement, scrolling and clicking are
never affected.

| Gesture | Action |
|---|---|
| **3 quick taps** | disable — overlay goes off screen, **stream keeps running** |
| **6 quick taps** | enable — bring it back |
| **1 finger, press and hold still** | opacity **up**, continuously |
| **2 fingers, press and hold still** | opacity **down**, continuously |
| **2 fingers held still, then slide** | **move** the overlay |

Hide is quick (3 taps); revealing takes a clearly intentional burst (6 taps) so a
stray triple-click can never put the overlay back on screen. Every gesture needs
the fingers to hold still briefly first, so a scroll — which starts moving
immediately — is never mistaken for one. Check support with `ooi probe-touchpad`;
disable gestures with `--no-gestures`.

---

## Flags

```
ooi start   (alias: serve)
  --geometry 1280x720      overlay size, or WxH+X+Y for an explicit position
  --opacity 235            1-255
  --codecs vp9,av1,vp8,h264   codec preference offered to the browser
  --threads 0              decoder threads (0 = auto)
  --service <url>          rendezvous base URL (default share.mdarif.online/ooi)
  --turn turn:host:port    TURN relay for when a direct path is impossible
  --turn-user / --turn-pass  TURN credentials
  --no-stun                LAN-only, no external STUN contact
  --new-code               rotate the join code before starting
  --click-through          let mouse clicks pass through (default true)
  --no-hotkeys             do not register global hotkeys
  --no-gestures            disable touchpad gesture control
  --foreground             stay in this terminal with a live log (default: background)
  --log-file <path>        log to a file (the background default is %LOCALAPPDATA%\ooi\ooi.log)
  --quiet                  suppress the log stream
```

Geometry is in **physical pixels** — the process is per-monitor DPI aware, so
`1280x720` is 1280×720 real pixels at any display scaling.

---

## Architecture

```
FRIEND (browser)                         YOU (ooi.exe)
┌────────────────────────┐  WebRTC     ┌──────────────────────────────────┐
│ getDisplayMedia()      │  (SRTP,     │ Pion WebRTC (offerer, recvonly)   │
│  → VP9/AV1 encode      │   direct,   │  → NACK/RTX + TWCC interceptors   │
│  → maintain-resolution │   P2P)      │  → jitter reassembly (loss policy)│
│  → RTP  ───────────────┼───────────► │  → native decode (libvpx/dav1d/…) │
└────────────────────────┘             │  → antialiased YUV→BGRA resample  │
        ▲ sealed offer/answer          │  → UpdateLayeredWindow            │
        │ (AES-256-GCM, code-derived)  │  → WDA_EXCLUDEFROMCAPTURE (always)│
   Cloudflare Worker + Durable Object  └──────────────────────────────────┘
   (rendezvous only; never sees media)
```

| Package | Responsibility |
|---|---|
| `internal/code` | join-code alphabet, generation, persistence, room ids |
| `internal/seal` | AES-256-GCM sealed offer/answer, byte-compatible with the browser |
| `internal/rendezvous` | HTTP client for the Cloudflare mailbox |
| `internal/rtc` | Pion peer connection, media engine, NACK/RTX/TWCC, reconnect driver |
| `internal/jitter` | RTP → complete-frame reassembly with a retransmit-aware loss policy |
| `internal/vdec` | cgo decoders (libvpx, dav1d, openh264) + antialiased resampler |
| `internal/overlay` | the capture-excluded layered window, gestures, fail-closed show |
| `internal/capture`, `internal/verify` | the A/B capture-protection measurement |
| `internal/winapi`, `internal/touchpad`, `internal/gesture` | Win32, HID, gesture state machine |
| `rendezvous/` | the Cloudflare Worker + Durable Object and the browser share page |

---

## Building from source (developers only)

Users never need this — releases are built by GitHub Actions
([.github/workflows/release.yml](.github/workflows/release.yml)) on every `v*` tag.

```powershell
.\build.ps1                 # produces ooi.exe (needs MSYS2 mingw64 gcc)
.\build.ps1 -Test           # build + run the offline test suite
```

```powershell
go test -short ./internal/...   # pure logic + decoders, no window
go test ./internal/...          # full suite incl. real capture A/B sweep
go test -run TestEndToEnd .     # full stack over the deployed rendezvous (needs network)
```

Decoding, colour and resampling are tested against **real encoded bitstreams**;
the sealed-blob format is tested **cross-implementation** against the browser's
WebCrypto; the rendezvous has a Node script that drives both peers against the
deployed Worker; and the end-to-end test runs the whole receiver against a
headless sender over the real internet path.

See [SETUP.md](SETUP.md) for install options, troubleshooting, and maintainer notes.
