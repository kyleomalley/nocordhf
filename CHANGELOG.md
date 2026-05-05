# Changelog

All notable changes to NocordHF are tracked in this file. Version
numbers follow [Semantic Versioning](https://semver.org/).

## [1.1.0] - 2026-05-05

### MeshCore (new mode)

- Mode rail gains a MESH chip alongside FT8 (FT4 stays the
  "coming soon" placeholder). Selecting it switches the active
  mode, hides the FT8 waterfall, gives the map the full
  right-hand pane, and swaps the channel column from the bands
  list to a Contacts + Channels sidebar. Selection persists
  across launches via the `active_mode` preference; the chip
  palette repaints so the active mode is the only Discord-blurple
  chip on the rail.
- New `lib/meshcore/` package — full companion-radio modem in
  pure Go, modelled on `liamcottle/meshcore.js`. Supports the
  serial framing protocol (`<` outgoing / `>` incoming with
  uint16-LE length prefix), AppStart handshake, GetContacts /
  GetChannels enumeration, SendContactMessage /
  SendChannelMessage, SyncNextMessage drain, SetDeviceTime,
  and an event stream for adverts, msg-waiting pushes, and
  send confirmations. Frame reader handles partial reads,
  byte-stream resync after a noisy boot banner, and a 8-KiB
  payload cap to fail fast on a stray length field.
- Known-board table for USB-attached LoRa devboards (Heltec V3,
  LilyGO T-Beam / T-Deck, RAK4631, Adafruit / Seeed nRF52840 +
  LoRa, generic). Settings → MeshCore tab picks board / port /
  baud; persisted as `meshcore_board`, `meshcore_port`,
  `meshcore_baud`.
- Mode entry lazy-opens the configured device, runs AppStart,
  syncs the device clock (so per-message senderTimestamp is
  meaningful on RTC-less boards), and pulls the contact +
  channel tables. Sidebar shows "CONTACTS (n)" with one row per
  contact (R/M/S marker for Repeater/Room/Sensor types), a
  divider, and "CHANNELS (n)" with one row per configured
  channel. Selecting a contact / channel switches the chat to
  that thread; the input field sends to the active selection.
- Per-thread chat history is preserved across thread + mode
  flips. Switching FT8 → MeshCore stashes the FT8 chat buffer
  and restores it on the way back; switching threads within
  MeshCore swaps in that thread's saved rows. Inbound messages
  arriving for a non-active thread are buffered silently in
  the background and surfaced when the operator selects them.
- Topic bar in MeshCore mode shows "DM with X" for a contact
  thread or "#channel" for a channel thread instead of the FT8
  slot / NTP / TX status line.

### MeshCore chat + RxLog

- Per-message delivery state tracking. Outbound contact + channel
  messages now land as Pending (`⏳ sending…`), flip to Delivered
  (`✓ delivered`) when the firmware emits `PushSendConfirmed` with
  a matching ack-CRC, or to Failed (`✗ failed`) after 90 s without
  confirmation. Status sweep runs from the existing 1 Hz ticker.
- New RxLog pane in MeshCore mode — sits below the map in a draggable
  VSplit and shows every mesh packet the radio decodes off-air. One
  row per packet: time, route type (FLOOD / DIRECT / TRANSPORT_*),
  payload type (TXT_MSG / ADVERT / PATH / ACK / …), hop count, SNR,
  RSSI. Ring-buffer-capped at 200 entries; newest at the top.
- New `lib/meshcore/packet.go` — pure-Go port of liamcottle/meshcore.js
  `packet.js`. `PacketFromBytes` decodes the mesh-layer header
  (route + payload type + version bits), transport codes, path
  hashes, and payload. Round-trip tests for FLOOD TXT_MSG and
  TRANSPORT_FLOOD ADVERT shapes lock the byte layout.
- New `meshcore.EventRxLog` event carrying SNR/RSSI + parsed
  `Packet` for every `PushLogRxData` push the firmware emits.
  Previously logged but unused; now drives the new viewer.

### MeshCore transport

- Bluetooth Low Energy support — connect to MeshCore companion
  devices that ship with the BLE-only firmware (LilyGO T1000-E,
  most nRF52840 trackers, anything you'd otherwise drive from
  the official mobile / web apps). New `lib/meshcore` `Transport`
  interface abstracts the link layer; existing serial path moved
  behind `serialTransport`, BLE path is `bleTransport` (Nordic
  UART Service GATT — service `6e400001-…`, write to `…0002`,
  notify on `…0003`).
- Settings → MeshCore → Device gains a Transport picker (USB
  Serial / Bluetooth) at the top with a swappable sub-form. The
  Bluetooth side has a Scan… button that opens a modal listing
  every nearby peripheral advertising the MeshCore service UUID;
  tap one to select, tap Connect now (or Save) to use it. Both
  sides' state persists independently, so flipping the radio
  picker doesn't lose the unused side's pick.
- New `meshcore.transport`, `meshcore.ble.address`,
  `meshcore.ble.device_name` preference keys. macOS bundle gains
  `NSBluetoothAlwaysUsageDescription` Info.plist entry +
  `com.apple.security.device.bluetooth` entitlement (parallel to
  the audio-input pair shipped in 1.0.5).
- New `tinygo.org/x/bluetooth` dependency for cross-platform GATT
  (CoreBluetooth on macOS, BlueZ on Linux, WinRT on Windows).
- `meshcore.ScanBLE(duration)` discovers peripherals advertising
  the MeshCore service UUID; `meshcore.OpenBLE(address, timeout)`
  reconnects to a saved peripheral. `meshcore.OpenSerial` is the
  renamed serial constructor (was `Open`).

### Settings

- Settings dialog is now mode-contextual. Tapping the gear in FT8
  mode opens the FT8 dialog (Profile, Radio, Map / Decoder, LoTW,
  TQSL Upload); tapping it in MeshCore mode opens a focused
  MeshCore dialog (Device, Profile). Each mode sees only the
  knobs that are relevant to it — no mixed concerns, no scrolling
  past inapplicable tabs.
- MeshCore prefs are namespaced under `meshcore.device.*` and
  `meshcore.profile.*` so they can never collide with FT8's
  flat-key prefs. (No migration: MeshCore was new in this
  release, so the old short-lived `meshcore_board` /
  `meshcore_port` / `meshcore_baud` keys are simply ignored.)
- MeshCore Profile tab persists `meshcore.profile.name`,
  `meshcore.profile.lat`, `meshcore.profile.lon` and pushes them
  to the live device on Save (or on the next connect when the
  client isn't open). Includes a "Send self-advert" button so the
  operator can announce the change without waiting for the
  firmware's periodic advert.

### Fixes

- FT8 chat no longer bleeds into MeshCore mode. `appendRow`
  parks decode / TX rows in `ft8RowsBackup` instead of `g.rows`
  when the active mode is MeshCore (system rows still surface
  live as operator notifications). `mcAppendRow` likewise gates
  live-view writes on both the active-mode AND active-thread
  matching, so an inbound MeshCore message arriving for a
  non-active thread or a different mode is buffered silently in
  history.

### Scope

- `scopePane.SetWaterfallVisible(bool)` swaps the right
  pane between the FT8 waterfall+map VSplit and a
  map-only layout. Same `MapWidget` instance in both
  modes so worked-grid overlay state, spot pins, and
  the QSO partner arc all carry through a mode flip.
- MeshCore scope layout: RxLog moved to the top of the
  right pane, map underneath. Newest RxLog entry at the
  bottom of the list with autoscroll-to-bottom on append.

### MeshCore chat

- @-mention tab completion. Type `@`, press Tab to insert
  the first matching contact name (alphabetical, case-
  insensitive). Repeated Tab cycles. Inserts as
  `@[Name] ` so other clients render the mention.
- Inbound `@[Name]` mentions render as bracket-stripped
  styled spans — cool blue for someone-pinged-someone,
  warm amber for "you got pinged".
- Mention-of-self threads flagged in the sidebar with
  `(@N)` and amber bold text instead of plain `(N)`.
- Inline path-hash links: comma-separated 2/4/6-byte
  hex series (`df,b7,43`) auto-detected against the
  contact roster. Matched hops become clickable links
  that fly the map to that contact.
- Inline URL links: `http(s)://` URLs render underlined
  and open in the system browser on click. Right-click
  for Open / Copy.
- 140-byte text length pre-validated before send so
  oversize messages fail fast instead of silently
  timing out at the firmware. Live `N/140` character
  counter at the right edge of the input — amber within
  10 of the cap, red over.
- Hover any timestamp (chat row or RxLog row) for the
  full local datetime tooltip.
- `(sending...)` → `(delivered)` / `(failed)` delivery
  state footer on tracked TX rows.
- Persistent path-trace data on `StoredMessage` —
  bytes captured at receive time and stored in bbolt so
  the chat-row right-click "Map Trace" works on
  historical messages after a relaunch.
- Chat-row right-click: Info (full route, SNR, delivery,
  persisted path), Map Trace (animate the route the
  message took).

### MeshCore contacts + channels

- Persistent favorite stars on contact rows. Click the
  star to toggle; favorites pin to the top of any sort.
- Distance sort (haversine from operator's broadcast
  position).
- Bulk delete dialog with presets: stale > 7d / 30d,
  never heard, broken timestamps.
- Manual-add-contacts toggle in Profile — when on, the
  radio stops auto-adding every advert it hears.
- Reset path action on the contact + map-node right-
  click menus. Clears the radio's cached out-route so
  the next DM re-discovers via FLOOD.
- Auto path reset after `mcAutoResetThreshold = 2`
  consecutive failed DMs to the same contact. Surfaces
  in chat as a system line; counter clears on any
  successful delivery or a manual reset.
- `PushPathUpdated` surfaces as a "path updated for
  Name" system line and triggers a debounced contact
  refresh so `Contact.OutPathLen` / `OutPath` stay
  fresh.
- Hashtag channel auto-derive: `Add hashtag channel`
  computes the secret as `SHA-256(name)[:16]` so
  community channels (`#volcano`, `#meshbud`, …) join
  by name.
- `+` button in the Channels header opens a popup with
  Add hashtag channel / Add private channel.
- Channel message routing tries both interpretations of
  the firmware's channel-id byte (slot index AND
  `SHA-256(secret)[0]`) and falls back to a synthetic
  `channel:unknown:XX` thread when neither resolves.
  Diagnostic log per route decision.
- `SyncNextMessage` decode errors now surface as system
  lines instead of being silently swallowed.

### MeshCore map

- Self-position pinned to the radio's GNSS-derived
  `SelfInfo.AdvLatE6/AdvLonE6` (yellow diamond),
  separate from the FT8 grid centroid.
- Curved-arc path animation. Each route segment draws
  progressively along a quadratic Bezier with an
  arrowhead at every completed hop. Total reveal is a
  fixed 2 s regardless of hop count; non-persistent
  paths fade over 5 s.
- Right-click any MeshCore node dot for Favorite, Info,
  Open chat, Show last path. Reuses the same context
  menu the contact sidebar offers.

### MeshCore connection

- Auto-reconnect on link drop. Configurable interval,
  default 5 minutes; 0 disables. Handles macOS
  sleep/wake gracefully — the BLE link silently dies
  during sleep and we now surface a "link dropped"
  system line and retry on schedule.
- Transport write failures close the underlying
  connection so `EventDisconnected` fires exactly once
  instead of every subsequent send timing out.

### Settings

- Profile tab: lat/lon picker. **Use radio GPS** snaps
  the entries from the radio's GNSS chip;
  **Pick on map…** opens a modal map-widget where a
  click drops the diamond. Manual edits override.

### Chrome

- Mode rail simplified to FT8 + MESH (FT4 placeholder
  removed).
- Hover tooltips use a non-blocking primitive overlay
  (instead of `widget.PopUp` which intercepted clicks)
  with a 400 ms debounce.
- Map widget refreshes always dispatch via `fyne.Do`
  (eliminates "Error in Fyne call thread" warnings on
  off-UI-thread setter calls).
- Star SVG icon (`internal/nocord/assets/star.svg` +
  `star_filled.svg`) replaces the Unicode glyph used
  for the contact-row favorite control.
- System messages and chat-row delivery footers no
  longer use Unicode glyph decorations.

### Documentation

- README rewrite: Getting Started section with macOS
  release / build-from-source / per-mode first-run
  config. Per-mode FT8 + MeshCore detail sections.
  Compatible-radios + compatible-boards tables.

## [1.0.8] - 2026-05-03

### Map

- HamDB auto-lookup on every decode upgrades coarse-prefix map
  placement to the operator's actual home coordinates. First
  decode of a callsign in a session fires a background lookup
  (8 s timeout, on-disk cache + per-call in-flight dedupe baked
  into the client); cached entries apply inline. Portable stations
  transmitting from a grid different to their home QTH are NOT
  overwritten — the message grid wins (`UpgradeSpotLocation`
  already had this guard).

### Cleanup

- Reverted v1.0.7's "double-tap on waterfall cancels queued
  retries" behaviour. Moving around the waterfall is just
  frequency selection, not a takeover gesture; **Esc** remains
  the only explicit cancel.

## [1.0.7] - 2026-05-02

### Manual takeover

- **Esc** anywhere on the window cancels every TX in flight or
  queued (active playback, anything sitting in `txCh`, all pending
  auto-reply retries) and clears the retry map. Use it whenever the
  auto-reply chain is going somewhere you don't want.
- **Double-tap** on the waterfall (the snap-once retune gesture)
  now also cancels everything in flight before retuning, so a stale
  retry doesn't fire on top of the operator's manual move. Drag
  remains gesture-only.

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
