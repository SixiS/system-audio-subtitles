#!/bin/bash
# Builds "System Audio Subtitles.app" (universal arm64+x86_64) and, when
# signing credentials are provided, produces a signed + notarized DMG ready
# for a GitHub release.
#
#   scripts/release.sh                 ad-hoc dev build → dist/ (local testing)
#   CODESIGN_IDENTITY="Developer ID Application: Name (TEAMID)" \
#   NOTARY_PROFILE=sas-notary \
#   scripts/release.sh v0.1.0          signed + notarized DMG → dist/
#
# See RELEASING.md for the one-time Apple Developer setup.
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${1:-0.0.0-dev}"
VERSION="${VERSION#v}"
APP="System Audio Subtitles"
DIST=dist
BUILD=$DIST/build
APPDIR="$DIST/$APP.app"
MACOS_MIN=14.4

rm -rf "$DIST"
mkdir -p "$BUILD"

echo "==> building universal binaries"
for arch in arm64 x86_64; do
	swiftc -O -target "$arch-apple-macos$MACOS_MIN" helper/audiotap.swift -o "$BUILD/audiotap.$arch" \
		-Xlinker -sectcreate -Xlinker __TEXT -Xlinker __info_plist -Xlinker helper/Info.plist
	swiftc -O -target "$arch-apple-macos$MACOS_MIN" helper/subtitlewindow.swift -o "$BUILD/subtitle-window.$arch"
done
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w" -o "$BUILD/sas.arm64" .
CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags="-s -w" -o "$BUILD/sas.x86_64" .
for bin in sas audiotap subtitle-window; do
	lipo -create "$BUILD/$bin.arm64" "$BUILD/$bin.x86_64" -output "$BUILD/$bin"
done

echo "==> assembling $APPDIR"
mkdir -p "$APPDIR/Contents/MacOS" "$APPDIR/Contents/Resources"
cp "$BUILD/sas" "$BUILD/audiotap" "$BUILD/subtitle-window" "$APPDIR/Contents/MacOS/"
sed "s/__VERSION__/$VERSION/g" packaging/Info.plist > "$APPDIR/Contents/Info.plist"
cp assets/icon.icns "$APPDIR/Contents/Resources/icon.icns"

# Sign inside-out: nested binaries first, then the bundle (which covers the
# main executable). Ad-hoc ("-") keeps local dev builds runnable; a real
# release needs Developer ID + hardened runtime + a secure timestamp or
# notarization will reject it.
IDENTITY="${CODESIGN_IDENTITY:--}"
if [ "$IDENTITY" = "-" ]; then
	echo "==> signing ad-hoc (set CODESIGN_IDENTITY for a distributable build)"
	sign() { codesign --force --sign - "$@"; }
else
	echo "==> signing as: $IDENTITY"
	sign() { codesign --force --options runtime --timestamp --sign "$IDENTITY" "$@"; }
fi
sign --identifier io.github.sixis.sas.audiotap \
	--entitlements packaging/audiotap.entitlements \
	"$APPDIR/Contents/MacOS/audiotap"
sign --identifier io.github.sixis.sas.subtitle-window "$APPDIR/Contents/MacOS/subtitle-window"
sign "$APPDIR"

if [ "$IDENTITY" = "-" ]; then
	echo "==> dev build ready: $APPDIR (unsigned — Gatekeeper will block downloads of this)"
	exit 0
fi

: "${NOTARY_PROFILE:?set NOTARY_PROFILE (see RELEASING.md: xcrun notarytool store-credentials)}"

echo "==> notarizing the app"
ZIP="$DIST/notarize.zip"
ditto -c -k --keepParent "$APPDIR" "$ZIP"
xcrun notarytool submit "$ZIP" --keychain-profile "$NOTARY_PROFILE" --wait
xcrun stapler staple "$APPDIR"
rm "$ZIP"

echo "==> building the DMG"
DMG="$DIST/SystemAudioSubtitles-$VERSION.dmg"
STAGE="$DIST/dmg-stage"
mkdir -p "$STAGE"
cp -R "$APPDIR" "$STAGE/"
ln -s /Applications "$STAGE/Applications"
hdiutil create -volname "$APP" -srcfolder "$STAGE" -ov -format UDZO "$DMG"
rm -rf "$STAGE"
codesign --force --timestamp --sign "$IDENTITY" "$DMG"
xcrun notarytool submit "$DMG" --keychain-profile "$NOTARY_PROFILE" --wait
xcrun stapler staple "$DMG"

echo "==> release artifact: $DMG"
echo "    publish with: gh release create v$VERSION '$DMG' --title 'v$VERSION' --generate-notes"
