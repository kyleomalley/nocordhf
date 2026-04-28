.PHONY: build run clean release-snapshot icon precommit

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

clean:
	rm -rf ./build ./dist

# Run formatting, vet, build, and tests. Intended to be run before
# creating a git commit.
precommit:
	gofmt -l . | tee /dev/stderr | (! read)
	go vet ./...
	go build ./...
	go test ./...
