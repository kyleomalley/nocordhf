# macOS code signing — chain setup & troubleshooting

When `codesign` or `make release-mac` reports:

```
Warning: unable to build chain to self-signed root for signer
"Developer ID Application: <name>"
errSecInternalComponent
```

…despite a valid Developer ID Application identity being present
(`security find-identity -p codesigning -v` lists it), the issue is
the **certificate trust chain**, not the signing identity itself. The
error message is misleading — `errSecInternalComponent` is a generic
Security.framework error that frequently signals chain-building
failure on modern macOS rather than an actual internal fault.

## The chain Apple requires

Developer ID code signing has three certificates:

1. **Leaf** — `Developer ID Application: <Your Name> (TEAMID)`,
   issued to you. Lives in your login keychain alongside its private
   key.
2. **Intermediate** — `Developer ID Certification Authority` (G2 as
   of writing), issued by Apple. **Not preinstalled** on macOS.
3. **Root** — `Apple Root CA` (the original SHA-1 root). Only
   `Apple Root CA - G3` is preinstalled in
   `/System/Library/Keychains/SystemRootCertificates.keychain` — the
   plain `Apple Root CA` that the G2 intermediate chains up to is
   **not preinstalled** on macOS 26 (Tahoe).

`codesign` at sign time must walk leaf → intermediate → root, with
the root trusted as an anchor in a keychain it consults. Missing any
link, or a revoked/expired leaf, produces the chain-building error.

### References

- Apple Developer — Distributing apps outside the Mac App Store:
  <https://developer.apple.com/documentation/security/notarizing_macos_software_before_distribution>
- Apple TN3147 — Migrating to the latest notarization tool:
  <https://developer.apple.com/documentation/technotes/tn3147-migrating-to-the-latest-notarization-tool>
- Apple Certificate Authority download index (intermediates + roots):
  <https://www.apple.com/certificateauthority/>
- Apple Inc. Root Certificates:
  <https://www.apple.com/appleca/>

`errSecInternalComponent` itself is documented under
`Security.framework` errors in the macOS SDK headers (`SecBase.h`).

## One-time setup on a new machine

1. **Download the Developer ID G2 intermediate** from Apple:
   ```sh
   curl -O https://www.apple.com/certificateauthority/DeveloperIDG2CA.cer
   ```
2. **Download the Apple Root CA** (plain — not G3):
   ```sh
   curl -O https://www.apple.com/appleca/AppleIncRootCertificate.cer
   ```
3. **Import both via Keychain Access** (drag-drop into the *login*
   keychain). Importing via the GUI registers the trust metadata
   that the headless `security add-certificates` doesn't always set
   correctly on Tahoe.
4. **Set trust** in Keychain Access:
   - Find each cert under **login → Certificates**.
   - Double-click → expand **Trust** → set "When using this
     certificate" to **Always Trust** for **Code Signing**, or leave
     at "Use System Defaults" if Apple's defaults already cover it.
   - macOS will prompt for your login password to save the trust
     change.
5. **Verify**:
   ```sh
   security find-certificate -c "Developer ID Certification Authority" | grep keychain
   security find-certificate -c "Apple Root CA" | grep keychain
   security trust-settings-export ~/tmp/trust.plist
   grep -A1 CodeSigning ~/tmp/trust.plist  # should show Result=1 (TrustRoot) on the relevant hashes
   ```
6. **Test sign**:
   ```sh
   echo test > /tmp/sigtest.txt
   codesign --force --options runtime --timestamp \
     --sign "Developer ID Application: <Your Name> (TEAMID)" \
     /tmp/sigtest.txt
   ```
   Should complete without `errSecInternalComponent`.

## If the chain is set up correctly but signing still fails

The most common remaining cause is that the **leaf certificate has
been revoked** by Apple — for example, after a security event on the
account, or because Apple invalidated a batch.

1. Open **Xcode → Settings → Accounts → your Apple ID → Manage
   Certificates…**
2. Look for `Developer ID Application` rows. A revoked cert is
   labelled **Revoked** in the list.
3. Delete the revoked entry (right-click → Delete) and click `+ →
   Developer ID Application` to issue a new one. Xcode handles the
   CSR + private key automatically.
4. After the new identity appears, retry `security find-identity -p
   codesigning -v` — the SHA-1 hash will be different, and the new
   identity should now sign cleanly.

A revoked leaf produces the same chain-building error as a missing
intermediate, which is why the diagnosis path is unintuitive.

## What was done in this project

During initial setup of `make release-mac`, signing failed with
`errSecInternalComponent` despite a valid-looking identity. The
sequence of fixes that resolved it:

1. Confirmed the leaf identity was present.
2. Identified the leaf's issuer as `Developer ID Certification
   Authority, OU=G2`.
3. Installed the G2 intermediate and the Apple Root CA into the
   login keychain.
4. Set explicit Code Signing trust on Apple Root CA via Keychain
   Access.
5. Discovered via Xcode → Manage Certificates that the existing
   Developer ID leaf was **revoked**. Deleted it and issued a fresh
   one. Signing then succeeded.
