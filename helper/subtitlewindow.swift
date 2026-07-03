import AppKit
import Foundation

// subtitle-window: a floating, borderless subtitle overlay. Reads JSON lines
// on stdin and re-renders on each one; terminates on stdin EOF.
//
//   {"segments":[{"source":"…","translation":"…","final":false}, …]}
//
// "source" is optional (present with --show-original). Non-final segments are
// rendered slightly transparent with a trailing ellipsis.

struct Seg: Decodable {
    let source: String?
    let translation: String
    let final: Bool
}

struct Payload: Decodable {
    let segments: [Seg]
}

let app = NSApplication.shared
app.setActivationPolicy(.accessory)

let screen = NSScreen.main?.visibleFrame ?? NSRect(x: 0, y: 0, width: 1440, height: 900)
let panelWidth = min(920, screen.width * 0.72)
let textInset: CGFloat = 16

let panel = NSPanel(
    contentRect: NSRect(x: screen.midX - panelWidth / 2, y: screen.minY + 80,
                        width: panelWidth, height: 80),
    styleMask: [.borderless, .nonactivatingPanel],
    backing: .buffered, defer: false)
panel.level = .floating
panel.isOpaque = false
panel.backgroundColor = .clear
panel.hasShadow = true
panel.isMovableByWindowBackground = true
panel.becomesKeyOnlyIfNeeded = true
panel.collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary]
panel.hidesOnDeactivate = false

let container = NSView()
container.wantsLayer = true
container.layer?.backgroundColor = NSColor.black.withAlphaComponent(0.72).cgColor
container.layer?.cornerRadius = 12
container.layer?.masksToBounds = true

// Slim top bar: drag handle + close button. Closing quits this process, which
// the pipeline notices (stdin peer gone) and shuts down cleanly.
let barHeight: CGFloat = 26
let bar = NSView()
bar.wantsLayer = true
bar.layer?.backgroundColor = NSColor.white.withAlphaComponent(0.06).cgColor
bar.translatesAutoresizingMaskIntoConstraints = false
container.addSubview(bar)

let closeButton = NSButton()
closeButton.isBordered = false
closeButton.image = NSImage(systemSymbolName: "xmark.circle.fill", accessibilityDescription: "Close")
closeButton.symbolConfiguration = NSImage.SymbolConfiguration(pointSize: 13, weight: .regular)
closeButton.contentTintColor = NSColor.white.withAlphaComponent(0.55)
closeButton.target = NSApp
closeButton.action = #selector(NSApplication.terminate(_:))
closeButton.translatesAutoresizingMaskIntoConstraints = false
bar.addSubview(closeButton)

let label = NSTextField(wrappingLabelWithString: "listening…")
label.font = NSFont.systemFont(ofSize: 20, weight: .medium)
label.textColor = NSColor.white.withAlphaComponent(0.6)
label.alignment = .center
label.preferredMaxLayoutWidth = panelWidth - 2 * textInset
label.translatesAutoresizingMaskIntoConstraints = false
container.addSubview(label)
panel.contentView = container
NSLayoutConstraint.activate([
    bar.topAnchor.constraint(equalTo: container.topAnchor),
    bar.leadingAnchor.constraint(equalTo: container.leadingAnchor),
    bar.trailingAnchor.constraint(equalTo: container.trailingAnchor),
    bar.heightAnchor.constraint(equalToConstant: barHeight),
    closeButton.leadingAnchor.constraint(equalTo: bar.leadingAnchor, constant: 9),
    closeButton.centerYAnchor.constraint(equalTo: bar.centerYAnchor),
    label.leadingAnchor.constraint(equalTo: container.leadingAnchor, constant: textInset),
    label.trailingAnchor.constraint(equalTo: container.trailingAnchor, constant: -textInset),
    label.topAnchor.constraint(equalTo: bar.bottomAnchor, constant: 10),
    label.bottomAnchor.constraint(equalTo: container.bottomAnchor, constant: -textInset),
])
panel.orderFrontRegardless()

func render(_ payload: Payload) {
    let text = NSMutableAttributedString()
    let sourceFont = NSFont.systemFont(ofSize: 13)
    let mainFont = NSFont.systemFont(ofSize: 21, weight: .medium)

    // Tight spacing inside a source/translation pair, a clear gap after each
    // pair — done per-paragraph, since spacing is a paragraph attribute.
    let sourcePara = NSMutableParagraphStyle()
    sourcePara.alignment = .center
    sourcePara.lineSpacing = 2
    sourcePara.paragraphSpacing = 1
    let mainPara = NSMutableParagraphStyle()
    mainPara.alignment = .center
    mainPara.lineSpacing = 3
    mainPara.paragraphSpacing = 14

    for (index, seg) in payload.segments.enumerated() {
        if index > 0 {
            text.append(NSAttributedString(string: "\n", attributes: [.paragraphStyle: mainPara]))
        }
        if let source = seg.source, !source.isEmpty {
            text.append(NSAttributedString(
                string: source + "\n",
                attributes: [.foregroundColor: NSColor.white.withAlphaComponent(0.4),
                             .font: sourceFont,
                             .paragraphStyle: sourcePara]))
        }
        let line = seg.final ? seg.translation : seg.translation + " …"
        let color = seg.final ? NSColor.white : NSColor.white.withAlphaComponent(0.85)
        text.append(NSAttributedString(
            string: line,
            attributes: [.foregroundColor: color, .font: mainFont, .paragraphStyle: mainPara]))
    }
    if text.length == 0 {
        text.append(NSAttributedString(
            string: "listening…",
            attributes: [.foregroundColor: NSColor.white.withAlphaComponent(0.6),
                         .font: mainFont, .paragraphStyle: mainPara]))
    }

    label.attributedStringValue = text

    // Size from Auto Layout's own measurement — computing the height any
    // other way fights the constraints, and AppKit resolves that fight by
    // moving the window (it crawls off-screen one render at a time).
    var frame = panel.frame
    frame.size.width = panelWidth
    frame.size.height = min(max(56, container.fittingSize.height), screen.height / 2)
    // Bottom edge stays anchored; if the window ever ends up off-screen
    // (or dragged there), snap it back.
    if frame.origin.y < screen.minY || frame.origin.y > screen.maxY - 100 {
        frame.origin.y = screen.minY + 80
    }
    if frame.origin.x < screen.minX || frame.maxX > screen.maxX {
        frame.origin.x = screen.midX - panelWidth / 2
    }
    panel.setFrame(frame, display: true)
}

DispatchQueue.global().async {
    while let line = readLine(strippingNewline: true) {
        guard let data = line.data(using: .utf8),
              let payload = try? JSONDecoder().decode(Payload.self, from: data) else { continue }
        DispatchQueue.main.async { render(payload) }
    }
    DispatchQueue.main.async { app.terminate(nil) } // pipeline exited
}

app.run()
