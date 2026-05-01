# Changelog

All notable changes to NocordHF are tracked in this file. Version
numbers follow [Semantic Versioning](https://semver.org/).

## [1.0.4] - 2026-05-01

### Fixes

- Bundle launches (Finder double-click on `NocordHF.app`) crashed
  immediately because cwd was `/`, and `logging.InitFile` couldn't
  create `nocordhf.log` there. The app now `chdir`s to
  `~/Library/Application Support/NocordHF/` when launched as a
  bundle so log + ADIF + recordings have a writable home. Terminal
  launches keep the existing cwd.

### Release tooling

- `MAX_WAIT` in `scripts/notarize-wait.sh` raised from 30 min to
  1 hr. v1.0.3's DMG submission was accepted at 28m20s — only
  100 s clear of timing out.
- New `make release-staple` target picks up after a
  notarization-timeout failure: takes the existing submission ID,
  polls Apple, staples the `.app`, then builds + signs + notarizes
  + staples the DMG. Avoids a full rebuild + resubmit when Apple's
  queue is just slow.

## [1.0.3] - 2026-05-01

### Decoder

- Lower Costas sync threshold from 5 to 2 to match jt9's `syncmin`,
  recovering ~60–70% of the decodes the previous threshold filtered
  out before the rescue path even ran.
- Replace the hard candidate cap with a percentile cap (top 60% by
  sync score, bounded `[200, 600]`). Cuts decode wall time ~10×
  (27 s → 2.6 s/slot avg) with no measurable recall loss; F1 89.5%
  vs jt9 v2.8.0 across an 89-slot live corpus.

### Tooling

- New `nocordhf -decode -corpus-dir DIR` for offline batch decode of
  WAV files into a versioned corpus directory.
- `tools/jt9_corpus`, `tools/compare_corpus`, `tools/profile_decode`
  for building reference corpora and diffing decoder iterations over
  time. `make precommit` now runs `go test -short` so the long
  sensitivity sweep skips by default (it already self-gates).

### QSO automation

- `Auto` checkbox next to the chat input. When enabled, an inbound
  message addressed at us triggers the next-step reply automatically
  (sig report → R+report → RR73 → 73). Stateless — pure function of
  the message just heard, deduped per slot to absorb Costas-hit dups.
- Right-click Reply / profile-popup Reply now use the most recent
  inbound from that call to pick the correct trailer instead of
  always re-sending the calling-with-grid form.
- Chat input accepts `<CALL> <TAIL>` (e.g. `VP2MAA -10`) for manual
  QSO progression alongside bare `CQ` and `<CALL>`.
- TX period lock: a directed TX defers one slot if the next 15-s
  boundary would land in the target station's TX period, preventing
  same-slot collisions.
- Slot deferral notice fires immediately when the TX is queued,
  rather than after a misleading countdown to a slot we'd then skip.

### Chat highlighting

- Bright cyan for messages addressed to us (was amber, indistinguish-
  able from CQs in busy bands).
- Warm orange for rows where one of our open-QSO targets is talking
  with — or being called by — someone else; a "they're busy" cue.

### Map / scope

- Frequency-axis caret aligned with the actual waterfall column
  (was offset by ~64 px because the time-strip width wasn't being
  subtracted from the axis range).
- OTA legend box width sizes dynamically to its longest label so
  "WWFF Flora & Fauna" no longer overflows.

### TQSL

- Add `-q -y -n` to the upload invocation so batch uploads no longer
  pop the recurring "confirm grid square" dialog or require operator
  intervention on yes/no prompts.

### Fixes

- Auto-reply previously never fired: `remoteCallFromMessage`
  returns `fields[0]` (the recipient = us, for any message addressed
  at us), so the equality-with-myCall guard immediately bailed. Now
  uses `senderFromMessage` to extract the actual remote callsign.
- `Auto` checkbox state now persists across restarts. Was reading
  `g.autoReply` at checkbox-creation time, but `ApplySavedToggles`
  hydrated that field afterwards — so the visual always rendered
  off and was out of sync with the gated state.

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
