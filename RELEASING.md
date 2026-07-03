# Releasing

Releases are a signed + notarized `.app` inside a DMG, published on GitHub
Releases — friends download, drag to Applications, double-click, no Gatekeeper
warnings. Everything runs locally with `make release`; no CI required.

## One-time setup

1. **Join the Apple Developer Program** — <https://developer.apple.com/programs/enroll/>
   ($99/yr, approval usually within a day or two). Note your **Team ID**
   (Membership page, e.g. `AB12CD34EF`).

2. **Create a "Developer ID Application" certificate.**
   - Keychain Access → Certificate Assistant → *Request a Certificate From a
     Certificate Authority…* → save the CSR to disk.
   - <https://developer.apple.com/account/resources/certificates/add> →
     **Developer ID Application** → upload the CSR → download the `.cer` →
     double-click to install it in your login keychain.
   - Confirm it's usable: `security find-identity -v -p codesigning` should
     list `Developer ID Application: Your Name (TEAMID)`.

3. **Store notarization credentials** (needs an app-specific password from
   <https://account.apple.com> → Sign-In and Security → App-Specific Passwords):

   ```sh
   xcrun notarytool store-credentials sas-notary \
     --apple-id you@example.com --team-id TEAMID --password xxxx-xxxx-xxxx-xxxx
   ```

   `notarytool` and `stapler` ship with the Xcode Command Line Tools — full
   Xcode is not required.

## Cutting a release

```sh
export CODESIGN_IDENTITY="Developer ID Application: Your Name (TEAMID)"
export NOTARY_PROFILE=sas-notary

make release VERSION=v0.1.0
git tag v0.1.0 && git push origin v0.1.0
gh release create v0.1.0 "dist/SystemAudioSubtitles-0.1.0.dmg" --title v0.1.0 --generate-notes
```

`make release` builds universal (arm64 + x86_64) binaries, assembles
`System Audio Subtitles.app`, signs everything inside-out with the hardened
runtime, notarizes the app, staples the ticket, wraps it in a DMG, and
notarizes + staples that too. Notarization typically takes a couple of
minutes per submission.

## Local testing without a certificate

`make app` builds the same bundle ad-hoc-signed into `dist/` — it runs fine
on your own machine (`open "dist/System Audio Subtitles.app"`) but downloads
of it would be blocked by Gatekeeper, so it's for testing only.

## Notes

- Signing with a stable Developer ID identity also fixes the development
  annoyance where every audiotap rebuild re-triggered the System Audio
  Recording permission prompt: TCC grants stick to the signing identity, so
  released builds keep their permission across updates.
- The app is an agent (`LSUIElement`): no Dock icon; the floating window and
  the menu-bar captions item come from the bundled subtitle-window helper.
- The bundle layout is flat: `Contents/MacOS/{sas,audiotap,subtitle-window}`.
  `sas` finds its helpers next to its own executable, so the bundle works
  from any launch location (including Gatekeeper's app translocation).
