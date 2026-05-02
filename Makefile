.PHONY: build run clean release-snapshot release-mac release-staple icon precommit

# BuildID is stamped into the binary so a running NocordHF advertises
# its origin in the title bar / log lines. Local builds use a
# git-hash + UTC timestamp; release builds (via goreleaser) override
# this with the version tag (e.g. v1.0.0).
BUILD_ID := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)-$(shell date -u +%Y%m%d%H%M%S)
LDFLAGS  := -X main.BuildID=$(BUILD_ID)

build:
	go build -ldflags "$(LDFLAGS)" -o ./build/nocordhf ./cmd/nocordhf

run: build
	./build/nocordhf

# Render the app icon (`docs/icon.png`) from the in-tree generator.
# Run on demand when the design changes; the result is committed.
icon:
	go run ./scripts/genicon ./docs/icon.png

# Local dry-run of the release pipeline. Produces artefacts under
# `dist/` without uploading. Useful before pushing a real tag.
release-snapshot:
	goreleaser release --snapshot --clean

# Build a signed + notarized macOS .app + .dmg locally. Prereqs:
#
#   - Xcode command-line tools (codesign, xcrun, hdiutil)
#   - A Developer ID Application certificate in the login keychain
#   - A notarytool keychain profile created with:
#       xcrun notarytool store-credentials nocordhf-notary \
#         --apple-id <you@example.com> --team-id <TEAMID> \
#         --password <app-specific-password>
#   - The fyne CLI (auto-installed via `go install` if missing)
#
# Required env:
#   NOCORDHF_VERSION       e.g. 1.0.1-test or 1.0.1
#   MACOS_CERTIFICATE_NAME e.g. "Developer ID Application: Your Name (TEAMID)"
#
# Optional env:
#   NOTARY_PROFILE         keychain profile name (default: nocordhf-notary)
#
# Produces under ./build/:
#   NocordHF.app           signed + notarized + stapled
#   NocordHF-<ver>.dmg     signed + notarized + stapled DMG of the .app
NOTARY_PROFILE ?= nocordhf-notary
# `go install` drops binaries in $GOPATH/bin which often isn't on the
# default PATH for non-interactive shells; prepend it so a freshly-
# installed `fyne` is visible to the same target that installed it.
GOBIN := $(shell go env GOPATH)/bin
export PATH := $(GOBIN):$(PATH)
release-mac:
	@if [ -z "$(NOCORDHF_VERSION)" ]; then echo "NOCORDHF_VERSION is required"; exit 1; fi
	@if [ -z "$(MACOS_CERTIFICATE_NAME)" ]; then echo "MACOS_CERTIFICATE_NAME is required"; exit 1; fi
	@command -v fyne >/dev/null 2>&1 || { echo "installing fyne CLI"; go install fyne.io/tools/cmd/fyne@latest; }
	@command -v fyne >/dev/null 2>&1 || { echo "fyne still not on PATH after install — check $$(go env GOPATH)/bin"; exit 1; }
	@command -v codesign >/dev/null 2>&1 || { echo "codesign not found (install Xcode command-line tools)"; exit 1; }
	@command -v xcrun >/dev/null 2>&1 || { echo "xcrun not found (install Xcode command-line tools)"; exit 1; }
	@echo "==> cleaning previous build artefacts"
	rm -rf ./build/NocordHF.app ./build/NocordHF-$(NOCORDHF_VERSION).dmg ./build/NocordHF.zip
	mkdir -p ./build
	@echo "==> building universal binary (amd64 + arm64)"
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=1 \
		go build -trimpath -ldflags "-s -w -X main.BuildID=v$(NOCORDHF_VERSION)" \
		-o ./build/nocordhf-amd64 ./cmd/nocordhf
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 \
		go build -trimpath -ldflags "-s -w -X main.BuildID=v$(NOCORDHF_VERSION)" \
		-o ./build/nocordhf-arm64 ./cmd/nocordhf
	lipo -create -output ./build/nocordhf ./build/nocordhf-amd64 ./build/nocordhf-arm64
	rm ./build/nocordhf-amd64 ./build/nocordhf-arm64
	@echo "==> packaging .app via fyne"
	# `fyne package` rebuilds the binary even when --executable points
	# to one that already exists, silently overwriting our universal
	# lipo'd build with an amd64-only one stripped of our -ldflags
	# (no BuildID injection, runtime falls back to "nongit-..."). Two
	# defenses, both required:
	#   1. Stash the universal binary, then restore it into the .app
	#      bundle after fyne is done — preserves arm64 + amd64 fat.
	#   2. Export GOFLAGS so any rebuild fyne does carries the same
	#      ldflag we used above (so even fyne's amd64 build has the
	#      right BuildID, in case the cp fails for any reason).
	cp ./build/nocordhf ./build/nocordhf.universal
	cd ./build && \
		GOFLAGS='-ldflags=-X=main.BuildID=v$(NOCORDHF_VERSION)' \
		fyne package --target darwin --executable ./nocordhf --src ../cmd/nocordhf --icon $(CURDIR)/docs/icon.png --app-id com.nocordhf.app --name NocordHF --app-version $(NOCORDHF_VERSION) --release
	cp ./build/nocordhf.universal ./build/NocordHF.app/Contents/MacOS/nocordhf
	rm ./build/nocordhf ./build/nocordhf.universal
	@echo "==> patching Info.plist (NSMicrophoneUsageDescription, LSMinimumSystemVersion)"
	# fyne package silently drops the FyneApp.toml [macOS] section, so
	# without these keys the app gets a silent CoreAudio denial under
	# the hardened runtime and FT8 RX decodes nothing.
	plutil -insert NSMicrophoneUsageDescription -string "NocordHF captures audio from your radio's USB CODEC interface to decode FT8 transmissions." ./build/NocordHF.app/Contents/Info.plist
	plutil -replace LSMinimumSystemVersion -string "11.0" ./build/NocordHF.app/Contents/Info.plist
	@echo "==> codesigning .app with hardened runtime + audio-input entitlement"
	codesign --force --deep --options runtime --timestamp \
		--entitlements ./scripts/macos/entitlements.plist \
		--sign "$(MACOS_CERTIFICATE_NAME)" \
		./build/NocordHF.app
	codesign --verify --deep --strict --verbose=2 ./build/NocordHF.app
	@echo "==> zipping for notarization"
	cd ./build && ditto -c -k --keepParent NocordHF.app NocordHF.zip
	@echo "==> submitting .app.zip to notarytool"
	./scripts/notarize-wait.sh ./build/NocordHF.zip $(NOTARY_PROFILE)
	@echo "==> stapling notarization ticket"
	xcrun stapler staple ./build/NocordHF.app
	xcrun stapler validate ./build/NocordHF.app
	rm ./build/NocordHF.zip
	@echo "==> building DMG"
	hdiutil create -volname "NocordHF" -srcfolder ./build/NocordHF.app \
		-ov -format UDZO ./build/NocordHF-$(NOCORDHF_VERSION).dmg
	codesign --force --sign "$(MACOS_CERTIFICATE_NAME)" --timestamp \
		./build/NocordHF-$(NOCORDHF_VERSION).dmg
	@echo "==> notarizing DMG"
	./scripts/notarize-wait.sh ./build/NocordHF-$(NOCORDHF_VERSION).dmg $(NOTARY_PROFILE)
	xcrun stapler staple ./build/NocordHF-$(NOCORDHF_VERSION).dmg
	@echo
	@echo "✓ release-mac done:"
	@echo "    ./build/NocordHF.app"
	@echo "    ./build/NocordHF-$(NOCORDHF_VERSION).dmg"

# Resume a release that died because Apple's notary queue overran the
# CI runner's timeout (notarize-wait.sh exits at MAX_WAIT, taking the
# runner — and the in-flight submission's signed artefact — with it).
#
# Pre-requisites:
#   - The signed (but unstapled) ./build/NocordHF.app from the failed
#     run, OR a fresh local `make release-mac` re-run that got at
#     least to the `notarytool submit` step. The .app must be the same
#     bytes Apple has in their queue under APP_SUBMISSION_ID.
#   - APP_SUBMISSION_ID grabbed from the failed CI job's log (the
#     `submission id: <uuid>` line right before the timeout).
#   - The same NOCORDHF_VERSION + MACOS_CERTIFICATE_NAME the original
#     run used (otherwise the DMG won't match the .app's signature).
#   - A keychain profile named $NOTARY_PROFILE (default
#     `nocordhf-notary`) — use `xcrun notarytool store-credentials`
#     to set one up if it isn't already.
#
# What it does:
#   1. Polls Apple for APP_SUBMISSION_ID until Accepted (or fails out
#      on Invalid/Rejected).
#   2. Staples the existing .app.
#   3. Builds, signs, notarizes, and staples the DMG fresh — this
#      part submits a NEW submission ID for the DMG, then waits the
#      same way `release-mac` does.
#   4. Leaves both artefacts under ./build/ for manual upload to the
#      GitHub Release.
release-staple:
	@if [ -z "$(NOCORDHF_VERSION)" ]; then echo "NOCORDHF_VERSION is required"; exit 1; fi
	@if [ -z "$(MACOS_CERTIFICATE_NAME)" ]; then echo "MACOS_CERTIFICATE_NAME is required"; exit 1; fi
	@if [ -z "$(APP_SUBMISSION_ID)" ]; then echo "APP_SUBMISSION_ID is required (from the failed CI job log)"; exit 1; fi
	@if [ ! -d ./build/NocordHF.app ]; then echo "./build/NocordHF.app missing — re-run a local make release-mac up to the notarize step, or download the signed .app from the failed CI run"; exit 1; fi
	@echo "==> polling notary for $(APP_SUBMISSION_ID)"
	./scripts/notarize-wait.sh ./build/NocordHF.app $(NOTARY_PROFILE) $(APP_SUBMISSION_ID)
	@echo "==> stapling .app"
	xcrun stapler staple ./build/NocordHF.app
	xcrun stapler validate ./build/NocordHF.app
	@echo "==> building DMG"
	rm -f ./build/NocordHF-$(NOCORDHF_VERSION).dmg
	hdiutil create -volname "NocordHF" -srcfolder ./build/NocordHF.app \
		-ov -format UDZO ./build/NocordHF-$(NOCORDHF_VERSION).dmg
	codesign --force --sign "$(MACOS_CERTIFICATE_NAME)" --timestamp \
		./build/NocordHF-$(NOCORDHF_VERSION).dmg
	@echo "==> notarizing DMG (new submission)"
	./scripts/notarize-wait.sh ./build/NocordHF-$(NOCORDHF_VERSION).dmg $(NOTARY_PROFILE)
	xcrun stapler staple ./build/NocordHF-$(NOCORDHF_VERSION).dmg
	@echo
	@echo "✓ release-staple done. Upload manually to the draft Release:"
	@echo "    gh release upload v$(NOCORDHF_VERSION) \\"
	@echo "      ./build/NocordHF-$(NOCORDHF_VERSION).dmg \\"
	@echo "      ./build/NocordHF.app  (zip first: ditto -c -k --keepParent ./build/NocordHF.app ./build/NocordHF-$(NOCORDHF_VERSION).app.zip)"

clean:
	rm -rf ./build ./dist

# Run formatting, vet, build, and tests. Intended to be run before
# creating a git commit.
precommit:
	gofmt -l . | tee /dev/stderr | (! read)
	go vet ./...
	go build ./...
	go test -short ./...
