# NocordHF

A Discord-style chat-focused FT8 client for amateur radio. Built on top of
a self-contained FT8 modem implemented in pure Go.

![NocordHF main window](docs/screenshot-main.png)

![NocordHF scope with signal detection](docs/screenshot-scope.png)

## Features

- Pure-Go FT8 modem: encode, decode (BP + OSD + AP rescue), LDPC,
  full a-priori-decoder support.
- Live waterfall with snap-to-slot scaling, slot timestamps, and
  hollow identified decode boxes around every decoded signal.
- Single-click selection on a decoded signal: scrolls and
  highlights the matching chat + HEARD rows, freezes chat
  auto-scroll, and pins a magnification popup of the signal.
- Hover magnification of any decoded signal in the waterfall, with
  call/freq/SNR overlay.
- HEARD sidebar of recently decoded RX-only callsigns — sortable
  A-Z / SNR / NEW, with country flag, recent-CQ marker, and
  click-to-magnify.
- Right-click context menu on every callsign-bearing surface
  (chat, HEARD, waterfall box, map pin) — Profile, Reply / Call,
  Copy callsign, Open QRZ.
- Pannable / zoomable map with DXCC grid overlay (blue =
  worked-locally, yellow = LoTW QSO, red = LoTW QSL), station pins
  colored by worked status, callsign clusters circle-packed per
  cell so dense regions stay readable and stable across zoom.
- NTP drift indicator in the chat header (FT8 needs ±0.5 s).
- ADIF logging to `nocordhf.adif` driven by a passive RX-stream
  QSO tracker (no full FT8 handshake state machine — closes when a
  matching `RR73` / `73` is observed).
- LoTW download + upload: ARRL credentials in settings sync QSL
  records into the map overlay; TQSL credentials enable per-QSO
  auto-upload to LoTW.
- Operator-friendly defaults: PSKReporter activity counts in the
  band list, country tooltip on flag hover, stable map clustering,
  decode boxes that persist for the visible-history window.

## Compatible radios

CAT control auto-detects on the listed serial port. Audio uses
whatever device name you pass via `-audio`.

| Radio                        | CAT family | Tested cable                  |
|------------------------------|------------|-------------------------------|
| Icom IC-7300                 | CI-V       | Built-in USB (CP210x)         |
| Yaesu FT-891                 | CAT v2     | DigiRig + USB serial          |

Other Icom CI-V rigs (IC-705, IC-7610, IC-9700) should work with the
existing CI-V driver — only the radio-address byte differs and the
tester just needs to confirm. Other Yaesu CAT-v2 rigs (FT-991A,
FT-DX10) likewise share the protocol but haven't been verified on
hardware.

`-no-cat` runs RX-only with no PTT or tune support, useful for
pure-listener setups.

## Layout

- `cmd/nocordhf/` — application entrypoint.
- `internal/nocord/` — application UI (the Discord-style window).
- `lib/` — reusable packages, suitable for use in other Go ham-radio
  projects:
  - `ft8/` — FT8 modem (encode, decode, LDPC, OSD, AP).
  - `audio/` — capture + playback (malgo-backed).
  - `cat/` — radio CAT control (Icom CI-V, Yaesu).
  - `waterfall/` — live FFT spectrogram.
  - `mapview/` — pannable / zoomable spot map widget for Fyne.
  - `adif/`, `lotw/`, `tqsl/` — logging + LoTW upload pipeline.
  - `callsign/`, `hamdb/`, `pskreporter/` — callsign / station lookup
    + activity stats.
  - `ntpcheck/` — clock-drift probe (FT8 needs ±0.5 s).
  - `logging/` — zap-based structured logger.

## License

NocordHF is released under the GNU General Public License v3.0.
See [`LICENSE`](LICENSE) for the full text.

The FT8 modem in `lib/ft8/` is an independent, clean-room Go
implementation of the public FT8 protocol described in K9AN/G4WJS/K1JT,
*QEX* July/August 2020. Acknowledgements to WSJT-X, ft8_lib, and the
project's other upstream dependencies are in [`CREDITS.md`](CREDITS.md).

## Disclaimer

NocordHF transmits on amateur radio bands. You must hold a valid
amateur radio license, issued by your country's regulator, that
permits operation on the bands and modes you intend to use. The
operator is solely responsible for ensuring all transmissions comply
with applicable regulations (in the US: FCC Part 97; elsewhere as
applicable).

This software is distributed in the hope that it will be useful, but
WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. The authors are
not liable for any spurious emissions, missed contacts, illegal
transmissions, equipment damage, or any other harm arising from the
use of this software. See the GPL-3.0 LICENSE for the formal warranty
disclaimer.

Use at your own risk.

## Status

v1.0.0 — first release.
