# Changelog

All notable changes to NocordHF are tracked in this file. Version
numbers follow [Semantic Versioning](https://semver.org/).

## [1.0.1] - 2026-04-30

- Radio profile in Settings: persist type / port / baud, hot-swap
  without a restart, and start cleanly with no rig attached.
- Click-to-pin magnification popup on the waterfall with hover
  preview and `[QRZ] [Profile] [Call]` action buttons.
- Frequency-axis ruler with TX-frequency marker under the waterfall.
- SVG status badges per *OTA program (POTA, SOTA, WWFF, IOTA, BOTA,
  LOTA, NOTA, PORTABLE) plus a CQ badge, shown next to callsigns on
  the map and in the HEARD roster.
- Country flag emoji in the HEARD roster, gated against ISO-3166 so
  unsupported entities fall back to a 2-letter code.
- Reply-target parser fixed for `CQ NA …` / `CQ EU …` / numeric-zone
  CQ forms — modifier set is now shared with the FT8 decoder.
- New macOS dock icon and `make release-mac` for signed + notarized
  local builds.

## [1.0.0] - 2026-04-27

First independent release. Extracted from the ft8m8 monorepo as a
standalone application + reusable Go library.

### Added

- Discord-style chat UI for the FT8 receive stream, organised by band
  channel.
- Restricted TX model: only "CQ" or directed first-call, so malformed
  FT8 transmissions are impossible by construction.
- Pure-Go FT8 modem in `lib/ft8/` (encode, decode, LDPC, OSD, AP).
  No `ft8_lib` C dependency.
- Live waterfall with snap-to-slot scaling, slot timestamps, and
  hollow boxes around every decoded signal.
- Single-click selection on a decoded signal: scrolls + blink-
  highlights the matching chat / HEARD rows, freezes chat auto-scroll,
  pins a magnification popup of the signal.
- Hover magnification with call / freq / SNR overlay.
- HEARD sidebar (IRC-style nick list) with country flag, recent-CQ
  marker, and click-to-magnify; sortable A-Z / SNR / NEW.
- Right-click context menu on every callsign-bearing surface (chat,
  HEARD, waterfall box, map pin) with Profile, Reply / Call, Copy
  callsign, and Open QRZ actions.
- Operator profile dialog with country, distance + bearing, recent
  decodes, and one-click reply.
- Pannable / zoomable map with DXCC grid overlay (blue = worked
  locally, yellow = LoTW QSO, red = LoTW QSL). Callsign clusters are
  circle-packed per cell so dense regions stay readable and stable
  across zoom levels.
- NTP drift indicator in the chat header.
- ADIF logging to `nocordhf.adif` driven by a passive RX-stream QSO
  tracker.
- LoTW credentials in settings sync QSL records into the map overlay.
- TQSL credentials in settings enable per-QSO auto-upload to LoTW.
- PSKReporter activity counts in the band list.
- IC-7300 and FT-891 + DigiRig CAT auto-detection.
- GPL-3.0 license + amateur radio licensing disclaimer.
