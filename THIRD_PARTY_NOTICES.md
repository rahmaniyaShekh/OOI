# Third-party notices

`ooi.exe` statically links the following open-source components. Their
licenses permit redistribution in binary form provided these notices are kept.

## Video decoders (C, linked through cgo)

| Component | Version | License | Full text |
|---|---|---|---|
| libvpx (VP8/VP9) | 1.16.0 | BSD-3-Clause + patent grant | [LICENSE](third_party/x/mingw64/share/licenses/libvpx/LICENSE), [PATENTS](third_party/x/mingw64/share/licenses/libvpx/PATENTS) |
| dav1d (AV1) | 1.5.4 | BSD-2-Clause + patent grant | [COPYING](third_party/x/mingw64/share/licenses/dav1d/COPYING), [PATENTS](third_party/x/mingw64/share/licenses/dav1d/PATENTS) |
| OpenH264 (H.264) | 2.6.0 | BSD-2-Clause | [LICENSE](third_party/x/mingw64/share/licenses/openh264/LICENSE) |

The prebuilt static libraries and headers under `third_party/x/mingw64/` come
from the MSYS2 `mingw-w64-x86_64-libvpx`, `-dav1d` and `-openh264` packages.

The GCC runtime (libgcc, libstdc++, winpthreads) is linked statically under the
GCC Runtime Library Exception / MIT-style terms of MinGW-w64.

## Go modules

| Module | License |
|---|---|
| github.com/pion/* (webrtc, ice, dtls, srtp, sctp, rtp, rtcp, sdp, interceptor, stun, turn, mdns, transport, datachannel, logging, randutil) | MIT |
| golang.org/x/crypto, x/net, x/sys, x/text, x/time | BSD-3-Clause |
| github.com/google/uuid | BSD-3-Clause |
| github.com/wlynxg/anet | BSD-3-Clause |

The full license text of each Go module is in its source distribution
(`go mod download` then see the module directory).
