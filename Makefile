.PHONY: build run clean release-snapshot release-mac icon precommit

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
	@echo "==> packaging .app via fyne (uses FyneApp.toml for Info.plist)"
	cd ./build && fyne package --target darwin --executable ./nocordhf --src ../cmd/nocordhf --icon $(CURDIR)/docs/icon.png --app-id com.nocordhf.app --name NocordHF --app-version $(NOCORDHF_VERSION) --release
	rm ./build/nocordhf
	@echo "==> codesigning .app with hardened runtime"
	codesign --force --deep --options runtime --timestamp \
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

clean:
	rm -rf ./build ./dist

# Run formatting, vet, build, and tests. Intended to be run before
# creating a git commit.
precommit:
	gofmt -l . | tee /dev/stderr | (! read)
	go vet ./...
	go build ./...
	go test -short ./...
