import AppKit
import Foundation

// subtitle-window: a floating subtitle overlay. Reads one JSON message per
// line on stdin and terminates on stdin EOF:
//
//   {"type":"append","source":"…","translation":"…"}   finalized segment → history
//   {"type":"live","segments":[{"source":"…","translation":"…","final":false}]}
//
// Layout: [drag bar + close] / [scrollable history] / [pinned live area].
// The history scroll view sticks to the bottom unless the user has scrolled
// up to read; the live area never scrolls.

struct Seg: Decodable {
    let source: String?
    let translation: String
    let final: Bool
}

struct Msg: Decodable {
    let type: String
    let source: String?
    let translation: String?
    let time: String?
    let segments: [Seg]?
}

let app = NSApplication.shared
app.setActivationPolicy(.accessory)

let screen = NSScreen.main?.visibleFrame ?? NSRect(x: 0, y: 0, width: 1440, height: 900)
var panelWidth = min(920, screen.width * 0.72) // adopts the user's width after a manual resize
let textInset: CGFloat = 16
var historyHeight: CGFloat = 220 // ≈ three sections; manual resizes adjust this

// --- shared text styles ---

let sourceFont = NSFont.systemFont(ofSize: 13)
let mainFont = NSFont.systemFont(ofSize: 21, weight: .medium)

let sourcePara = NSMutableParagraphStyle()
sourcePara.alignment = .center
sourcePara.lineSpacing = 2
sourcePara.paragraphSpacing = 1

let mainPara = NSMutableParagraphStyle()
mainPara.alignment = .center
mainPara.lineSpacing = 3
mainPara.paragraphSpacing = 14

func sourceText(_ s: String) -> NSAttributedString {
    NSAttributedString(string: s, attributes: [
        .foregroundColor: NSColor.white.withAlphaComponent(0.4),
        .font: sourceFont, .paragraphStyle: sourcePara,
    ])
}

func timeText(_ s: String) -> NSAttributedString {
    NSAttributedString(string: s, attributes: [
        .foregroundColor: NSColor.white.withAlphaComponent(0.28),
        .font: NSFont.monospacedDigitSystemFont(ofSize: 10, weight: .regular),
        .paragraphStyle: sourcePara,
    ])
}

func mainText(_ s: String, alpha: CGFloat = 1.0) -> NSAttributedString {
    NSAttributedString(string: s, attributes: [
        .foregroundColor: NSColor.white.withAlphaComponent(alpha),
        .font: mainFont, .paragraphStyle: mainPara,
    ])
}

// Break after sentence-ending punctuation with a line separator (U+2028), not
// "\n" — a newline would start a new paragraph and pick up the big
// between-segments paragraphSpacing.
func sentenceBreaks(_ s: String) -> String {
    s.replacingOccurrences(of: #"(?<=[.!?。！？])\s+"#, with: "\u{2028}",
                           options: .regularExpression)
}

// --- window chrome ---

let panel = NSPanel(
    contentRect: NSRect(x: screen.midX - panelWidth / 2, y: screen.minY + 80,
                        width: panelWidth, height: 80),
    styleMask: [.borderless, .nonactivatingPanel, .resizable],
    backing: .buffered, defer: false)
panel.minSize = NSSize(width: 380, height: 140)
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

// --- scrollable history ---

let scroll = NSScrollView()
scroll.translatesAutoresizingMaskIntoConstraints = false
scroll.hasVerticalScroller = true
scroll.autohidesScrollers = true
scroll.scrollerStyle = .overlay
scroll.drawsBackground = false
container.addSubview(scroll)

let history = NSTextView(frame: NSRect(x: 0, y: 0, width: panelWidth, height: 0))
history.isEditable = false
// Not selectable: in a nonactivating panel the selection highlight gets stuck
// and turns the text into an unreadable light-on-light band.
history.isSelectable = false
history.drawsBackground = false
history.textContainerInset = NSSize(width: textInset - 5, height: 8)
history.isVerticallyResizable = true
history.isHorizontallyResizable = false
history.autoresizingMask = [.width]
history.textContainer?.widthTracksTextView = true
history.minSize = NSSize(width: 0, height: 0)
history.maxSize = NSSize(width: CGFloat.greatestFiniteMagnitude,
                         height: CGFloat.greatestFiniteMagnitude)
scroll.documentView = history

let divider = NSView()
divider.wantsLayer = true
divider.layer?.backgroundColor = NSColor.white.withAlphaComponent(0.08).cgColor
divider.translatesAutoresizingMaskIntoConstraints = false
divider.isHidden = true
container.addSubview(divider)

// --- pinned live area ---

let liveLabel = NSTextField(wrappingLabelWithString: "listening…")
// Wrapping labels are selectable by default; selection routes through the
// window's field editor, which mangles the styled runs on redraw.
liveLabel.isSelectable = false
liveLabel.textColor = NSColor.white.withAlphaComponent(0.6)
liveLabel.font = mainFont
liveLabel.alignment = .center
liveLabel.preferredMaxLayoutWidth = panelWidth - 2 * textInset
liveLabel.translatesAutoresizingMaskIntoConstraints = false
container.addSubview(liveLabel)

panel.contentView = container

// Below-required priority: a required height here would fully determine the
// window's height and AppKit would refuse vertical drags. At 400, the user's
// drag wins and the scroll area absorbs the height delta.
let scrollHeightConstraint = scroll.heightAnchor.constraint(equalToConstant: 0)
scrollHeightConstraint.priority = NSLayoutConstraint.Priority(rawValue: 400)
NSLayoutConstraint.activate([
    bar.topAnchor.constraint(equalTo: container.topAnchor),
    bar.leadingAnchor.constraint(equalTo: container.leadingAnchor),
    bar.trailingAnchor.constraint(equalTo: container.trailingAnchor),
    bar.heightAnchor.constraint(equalToConstant: barHeight),
    closeButton.leadingAnchor.constraint(equalTo: bar.leadingAnchor, constant: 9),
    closeButton.centerYAnchor.constraint(equalTo: bar.centerYAnchor),
    scroll.topAnchor.constraint(equalTo: bar.bottomAnchor),
    scroll.leadingAnchor.constraint(equalTo: container.leadingAnchor),
    scroll.trailingAnchor.constraint(equalTo: container.trailingAnchor),
    scrollHeightConstraint,
    divider.topAnchor.constraint(equalTo: scroll.bottomAnchor),
    divider.leadingAnchor.constraint(equalTo: container.leadingAnchor),
    divider.trailingAnchor.constraint(equalTo: container.trailingAnchor),
    divider.heightAnchor.constraint(equalToConstant: 1),
    liveLabel.topAnchor.constraint(equalTo: divider.bottomAnchor, constant: 10),
    liveLabel.leadingAnchor.constraint(equalTo: container.leadingAnchor, constant: textInset),
    liveLabel.trailingAnchor.constraint(equalTo: container.trailingAnchor, constant: -textInset),
    liveLabel.bottomAnchor.constraint(equalTo: container.bottomAnchor, constant: -textInset),
])
panel.orderFrontRegardless()

// --- rendering ---

var manualResizing = false

func relayout() {
    if panel.inLiveResize || manualResizing {
        return // the user's drag owns the frame right now
    }
    var frame = panel.frame
    frame.size.width = panelWidth
    frame.size.height = min(max(56, container.fittingSize.height), screen.height - 120)
    if frame.origin.y < screen.minY || frame.origin.y > screen.maxY - 100 {
        frame.origin.y = screen.minY + 80
    }
    if frame.origin.x < screen.minX || frame.maxX > screen.maxX {
        frame.origin.x = screen.midX - frame.size.width / 2
    }
    panel.setFrame(frame, display: true)
}

func historyIsAtBottom() -> Bool {
    let visible = scroll.contentView.bounds
    return visible.maxY >= history.frame.height - 40
}

var haveHistory = false

func appendHistory(source: String?, translation: String, time: String?) {
    let stick = historyIsAtBottom()
    let chunk = NSMutableAttributedString()
    if haveHistory {
        chunk.append(NSAttributedString(string: "\n", attributes: [.paragraphStyle: mainPara]))
    }
    if let time, !time.isEmpty {
        chunk.append(timeText("[" + time + "]\n"))
    }
    if let source, !source.isEmpty {
        chunk.append(sourceText(source + "\n"))
    }
    chunk.append(mainText(sentenceBreaks(translation)))
    history.textStorage?.append(chunk)
    if !haveHistory {
        haveHistory = true
        scrollHeightConstraint.constant = historyHeight
        divider.isHidden = false
    }
    relayout()
    if stick {
        history.scrollToEndOfDocument(nil)
    }
}

func renderLive(_ segments: [Seg]) {
    let text = NSMutableAttributedString()
    for (index, seg) in segments.enumerated() {
        if index > 0 {
            text.append(NSAttributedString(string: "\n", attributes: [.paragraphStyle: mainPara]))
        }
        if let source = seg.source, !source.isEmpty {
            text.append(sourceText(source + "\n"))
        }
        let line = sentenceBreaks(seg.final ? seg.translation : seg.translation + " …")
        text.append(mainText(line, alpha: seg.final ? 1.0 : 0.85))
    }
    if text.length == 0 {
        text.append(mainText(haveHistory ? "…" : "listening…", alpha: 0.6))
    }
    liveLabel.attributedStringValue = text
    relayout()
}

// Adopt a user-chosen size: the new width re-wraps text, and the height delta
// goes to the history viewport; the live area keeps auto-sizing.
func adoptManualSize() {
    let frame = panel.frame
    panelWidth = frame.width
    liveLabel.preferredMaxLayoutWidth = panelWidth - 2 * textInset
    if haveHistory {
        let chromeAndLive = container.fittingSize.height - scrollHeightConstraint.constant
        historyHeight = max(80, frame.height - chromeAndLive)
        scrollHeightConstraint.constant = historyHeight
    }
    relayout()
}

final class PanelDelegate: NSObject, NSWindowDelegate {
    func windowDidEndLiveResize(_ notification: Notification) {
        adoptManualSize()
    }
}
let panelDelegate = PanelDelegate()
panel.delegate = panelDelegate

// Explicit resize grip (bottom-right): borderless windows don't reliably get
// AppKit's invisible edge-resize bands in the vertical direction, so the grip
// tracks the drag itself.
final class ResizeGrip: NSView {
    override func draw(_ dirtyRect: NSRect) {
        NSColor.white.withAlphaComponent(0.35).setStroke()
        let path = NSBezierPath()
        path.lineWidth = 1
        for i in 0..<3 {
            let offset = CGFloat(4 + i * 4)
            path.move(to: NSPoint(x: bounds.maxX - offset, y: bounds.minY + 2))
            path.line(to: NSPoint(x: bounds.maxX - 2, y: bounds.minY + offset))
        }
        path.stroke()
    }

    override func mouseDown(with event: NSEvent) {
        guard let win = window else { return }
        manualResizing = true
        defer {
            manualResizing = false
            adoptManualSize()
        }
        let startFrame = win.frame
        let start = NSEvent.mouseLocation
        while true {
            guard let next = win.nextEvent(matching: [.leftMouseDragged, .leftMouseUp]) else { break }
            if next.type == .leftMouseUp {
                break
            }
            let loc = NSEvent.mouseLocation
            var f = startFrame
            f.size.width = max(380, startFrame.width + (loc.x - start.x))
            f.size.height = max(140, startFrame.height - (loc.y - start.y))
            f.origin.y = startFrame.maxY - f.size.height // top edge stays put
            win.setFrame(f, display: true)
        }
    }
}

let grip = ResizeGrip()
grip.translatesAutoresizingMaskIntoConstraints = false
container.addSubview(grip)
NSLayoutConstraint.activate([
    grip.widthAnchor.constraint(equalToConstant: 16),
    grip.heightAnchor.constraint(equalToConstant: 16),
    grip.trailingAnchor.constraint(equalTo: container.trailingAnchor, constant: -5),
    grip.bottomAnchor.constraint(equalTo: container.bottomAnchor, constant: -5),
])

DispatchQueue.global().async {
    while let line = readLine(strippingNewline: true) {
        guard let data = line.data(using: .utf8),
              let msg = try? JSONDecoder().decode(Msg.self, from: data) else { continue }
        DispatchQueue.main.async {
            switch msg.type {
            case "append":
                appendHistory(source: msg.source, translation: msg.translation ?? "", time: msg.time)
            case "live":
                renderLive(msg.segments ?? [])
            default:
                break
            }
        }
    }
    DispatchQueue.main.async { app.terminate(nil) } // pipeline exited
}

app.run()
