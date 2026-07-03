HELPER := bin/audiotap
WINDOW := bin/subtitle-window
GO_BIN := bin/sas

all: $(HELPER) $(WINDOW) $(GO_BIN)

# The Info.plist (bundle id + NSAudioCaptureUsageDescription for the TCC
# prompt) is embedded into the CLI binary's __TEXT,__info_plist section.
$(HELPER): helper/audiotap.swift helper/Info.plist
	@mkdir -p bin
	swiftc -O helper/audiotap.swift -o $@ \
		-Xlinker -sectcreate -Xlinker __TEXT -Xlinker __info_plist -Xlinker helper/Info.plist
	codesign --force --sign - --identifier io.github.sixis.sas.audiotap $@

$(WINDOW): helper/subtitlewindow.swift
	@mkdir -p bin
	swiftc -O helper/subtitlewindow.swift -o $@

$(GO_BIN): go.mod $(wildcard *.go)
	@mkdir -p bin
	go build -o $@ .

# Assemble a local, ad-hoc-signed app bundle in dist/ for testing.
app:
	scripts/release.sh

# Signed + notarized DMG; needs CODESIGN_IDENTITY and NOTARY_PROFILE set
# (one-time setup in RELEASING.md).
release:
	@test -n "$(VERSION)" || { echo "usage: make release VERSION=v0.1.0"; exit 1; }
	scripts/release.sh $(VERSION)

clean:
	rm -rf bin dist

.PHONY: all app release clean
