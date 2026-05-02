# Changelog

All notable changes to NocordHF are tracked in this file. Version
numbers follow [Semantic Versioning](https://semver.org/).

## [1.0.7] - 2026-05-02

### Manual takeover

- **Esc** anywhere on the window cancels every TX in flight or
  queued (active playback, anything sitting in `txCh`, all pending
  auto-reply retries) and clears the retry map. Use it whenever the
  auto-reply chain is going somewhere you don't want.

### Map

- HamDB auto-lookup on every decode upgrades coarse-prefix map
  placement to the operator's actual home coordinates. First
  decode of a callsign in a session fires a background lookup;
  cached entries apply inline. Portable stations transmitting from
  a grid different to their home QTH are NOT overwritten — the
  message grid wins (`UpgradeSpotLocation` already had this
  guard).

### Chat

- Tiny "?" badge next to the topic bar opens a reference dialog
  covering colour conventions, L/O badges, keyboard / mouse
  shortcuts, the Auto-progress chain, and chat-input shorthand.

### Fixes

- Stale auto-reply retries no longer fire after the QSO has moved
  on. The retry sweep can re-queue a TX (e.g. another `R-NN`) into
  `txCh` ~30 s after the original send; if the remote then sends us
  the next-step token in that window, `clearPendingRetry` removed
  the map entry but the queued TX kept playing — and the chat would
  show the operator going backwards in the QSO sequence (sending
  `R-4` after the QSO had already been logged from a `RR73`).
  `pendingRetry` now stashes the most recently queued TxRequest's
  `StopCh`; both `clearPendingRetry` and the next sweep close it
  before swapping in a new one, so `runTX`'s slot countdown /
  playback loop aborts the stale TX cleanly.

## [1.0.6] - 2026-05-01

### Map

- QSO propagation arc now drawn for the most recent inbound /
  outbound directed call. Cyan upward-bowing curve with arrowhead
  at the partner when we TX, amber downward-bowing curve with
  arrowhead back at us when they call. Latest event wins.
- "Roster stale (min)" pref in Settings → Profile (default 30,
  0 disables) — purges HEARD entries and map spots that haven't
  been refreshed in that window.
- Frequency-axis caret aligned to the actual waterfall column
  (was offset by ~64 px because the time-strip width wasn't
  subtracted from the axis range).

### Chat

- Live TX progress: the TX row appears the moment audio starts
  with the message split between green (already on-air) and grey
  (still pending). Lets the operator see what's transmitting in
  real time and stop early if needed.
- Drag the TX cursor across the waterfall to retune live (in
  addition to the existing double-click-to-snap).
- Prior-contact marker on every chat row: ★ for an LoTW QSL on
  the active band, ○ for an ADIF-only QSO. Skips when never
  worked.
- Topic bar shows current and previous slot decode counts
  ("rx 5/12") in bold, proportional font.
- Bright cyan for messages addressed to us; warm orange for rows
  where one of our open-QSO targets is talking with someone else.

### QSO automation

- Auto-reply retries up to 4 times (30 s apart) when the remote
  doesn't respond, then gives up. Skipped for terminal `73`
  (they've already closed; resending serves no purpose).
- Right-click Reply / profile-popup Reply use recent inbound
  context to pick the right next-step trailer instead of always
  re-sending the calling-with-grid form.
- TX period lock: directed TXs defer one slot if the next
  boundary would land in the target's TX period.

### Release tooling

- CI binary's `BuildID` now correctly reflects the release tag
  (was showing `nongit-…` because `fyne package` silently rebuilt
  the universal binary without our `-ldflags`). Stash the
  universal binary, restore it into the .app after fyne runs;
  also export `GOFLAGS` so any rebuild keeps the BuildID.
- `notarize-wait.sh` `MAX_WAIT` 30 min → 1 hr (v1.0.3's DMG
  finished at 28m20s — too tight).
- New `make release-staple` target: resume after a notary timeout
  by polling an existing submission ID instead of full rebuild.

### Decoder

- Diagnostic Info-level logs: `addr_us decode` whenever an
  inbound message addresses us (with auto-reply gate state +
  myCall), `qso-arc` on every map arc state change.

### Fixes

- Auto-reply previously never fired for any directed-at-us
  message because `remoteCallFromMessage` returns the recipient
  (us, for an addressed-at-us row) and the equality guard
  immediately bailed. Now uses `senderFromMessage`.
- Auto checkbox state survives restarts (was hydrated after the
  checkbox UI was built, so the visual rendered off and was out
  of sync with the gated state).

## [1.0.5] - 2026-05-01

### Fixes

- Bundled `.app` had no audio access. `fyne package` silently
  drops the `[macOS]` section in `FyneApp.toml`, so
  `NSMicrophoneUsageDescription` never reached `Info.plist` and
  CoreAudio refused mic capture under the hardened runtime. We now
  patch the key in via `plutil` after packaging and codesign with
  a `com.apple.security.device.audio-input` entitlement
  (`scripts/macos/entitlements.plist`). Affects FT8 RX from the
  rig's USB CODEC interface — without these the receive pipeline
  decoded nothing from a Finder-launched `.app`.
- TQSL upload exited 10 with a usage dump on every QSO. v1.0.3
  added `-y` and `-n` flags assuming TQSL 2.5/2.6 semantics; in
  TQSL 2.7+ `-y` doesn't exist and `-n` means `--updates`
  (triggers an update check). Reverted to the original
  `-x -u -d -a compliant` flag set.
- QSO closure double-uploaded to LoTW. The QSO tracker re-created
  an `openContact` when one was missing then immediately finalized
  it on `73`/`RR73`, so a repeat-decode of the closing message — or
  a manual TX of `73` after the auto-reply already sent one — fired
  `onLogged` twice. Now bails when the contact wasn't already open.

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
