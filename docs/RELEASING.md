# Releasing NocordHF

Until CI signing+notarization is wired up (see `release.yml`), macOS
releases are cut **locally** from a maintainer's machine that holds
the Developer ID signing identity in its keychain. This document
captures the full procedure.

## Prerequisites (one-time)

1. **Xcode command-line tools** — `codesign`, `xcrun`, `hdiutil`,
   `lipo`, `ditto`. Install with `xcode-select --install`.
2. **Developer ID Application certificate** in your login keychain.
   The leaf cert + private key must be present and not revoked. Verify:
   ```sh
   security find-identity -p codesigning -v
   ```
3. **Developer ID intermediate + Apple Root CA**. The G2 intermediate
   is *not* preinstalled on macOS; download from
   <https://www.apple.com/certificateauthority/> and import via
   Keychain Access. After import, set trust for Code Signing.
   See `docs/codesigning-troubleshooting.md` for the full chain
   walkthrough if `codesign` reports `errSecInternalComponent` or
   "unable to build chain to self-signed root".
4. **Notarization credentials stored in the keychain**:
   ```sh
   xcrun notarytool store-credentials nocordhf-notary \
     --apple-id you@example.com \
     --team-id YOURTEAMID \
     --password <app-specific-password>
   ```
   The profile name `nocordhf-notary` is what `make release-mac`
   uses by default; override with `NOTARY_PROFILE=...` if needed.
5. **fyne CLI** — auto-installed by the Makefile target on first run.

## Cutting a release

1. **Update CHANGELOG.md** with the new version, date, and bullets.
2. **Bump the version reference** in `FyneApp.toml` if it's still
   pointing at the prior release.
3. **Commit + merge** all release-bound changes to `main`.
4. **Tag the release** locally:
   ```sh
   git tag v1.0.1
   git push origin v1.0.1
   ```
   This kicks the existing GitHub Actions `release.yml` workflow,
   which currently produces an *ad-hoc-signed* DMG via goreleaser.
   That artefact is fine for users willing to right-click → Open past
   Gatekeeper, but is not the canonical macOS release.
5. **Build the canonical signed + notarized artefacts locally**:
   ```sh
   NOCORDHF_VERSION=1.0.1 \
   MACOS_CERTIFICATE_NAME="Developer ID Application: YOUR NAME (TEAMID)" \
   make release-mac
   ```
   The target produces `./build/NocordHF.app` (signed, notarized,
   stapled) and `./build/NocordHF-1.0.1.dmg` (signed, notarized,
   stapled). Notarization typically takes 5–15 minutes; Apple's queue
   occasionally spikes longer.
6. **Verify the artefacts**:
   ```sh
   spctl --assess --type exec -vv ./build/NocordHF.app
   xcrun stapler validate ./build/NocordHF-1.0.1.dmg
   ```
   Both should report acceptance. `spctl` should say
   `source=Notarized Developer ID`.
7. **Upload to the GitHub Release** that was auto-drafted by the tag
   push. Replace the goreleaser ad-hoc DMG with the notarized DMG
   from step 5. Publish the release.

## Verifying on a clean Mac

To sanity-check Gatekeeper acceptance on a machine that has never
seen this build before, copy the DMG over and:

```sh
xattr -p com.apple.quarantine /Volumes/NocordHF/NocordHF.app  # should report a quarantine flag
spctl --assess --type exec -vv /Volumes/NocordHF/NocordHF.app  # should be Notarized
```

If `spctl` says "rejected", the staple did not survive transit;
re-staple via `xcrun stapler staple` and re-zip the DMG.

## Rotating credentials

The notary profile (`xcrun notarytool store-credentials`) holds an
app-specific password. Rotate by generating a new one at
<https://appleid.apple.com/account/manage> → App-Specific Passwords,
then re-running `store-credentials` with the same profile name.

If your Developer ID Application certificate is revoked or expires,
revoke the local copy (Xcode → Settings → Accounts → Manage
Certificates), generate a new one, and re-export+upload to GitHub
Secrets if/when CI signing is enabled.
