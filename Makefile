HELPER := bin/audiotap
GO_BIN := bin/sas

all: $(HELPER) $(GO_BIN)

# The Info.plist (bundle id + NSAudioCaptureUsageDescription for the TCC
# prompt) is embedded into the CLI binary's __TEXT,__info_plist section.
$(HELPER): helper/audiotap.swift helper/Info.plist
	@mkdir -p bin
	swiftc -O helper/audiotap.swift -o $@ \
		-Xlinker -sectcreate -Xlinker __TEXT -Xlinker __info_plist -Xlinker helper/Info.plist
	codesign --force --sign - --identifier local.audiotap $@

$(GO_BIN): go.mod $(wildcard *.go)
	@mkdir -p bin
	go build -o $@ .

clean:
	rm -rf bin

.PHONY: all clean
