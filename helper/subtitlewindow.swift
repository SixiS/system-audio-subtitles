import AppKit
import Foundation

// subtitle-window: a floating subtitle overlay. Reads one JSON message per
// line on stdin and terminates on stdin EOF:
//
//   {"type":"append","source":"…","translation":"…"}   finalized segment → history
//   {"type":"live","segments":[{"source":"…","translation":"…","final":false}]}
//   {"type":"need_key"}                                 show the API-key prompt
//
// User actions are reported as JSON lines on stdout:
//
//   {"type":"key","key":"sk-…"}    key entered (first-boot prompt or settings)
//   {"type":"clear_key"}           clear the stored key and shut down
//
// A menu-bar status item and a gear in the window's top bar offer the settings
// menu: Edit API Key… / Clear API Key & Quit / Quit.
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
    let message: String?
}

let app = NSApplication.shared
app.setActivationPolicy(.accessory)

// Cmd-V & friends only work if an Edit menu supplies the standard key
// equivalents — script apps have no main menu by default, which made paste
// into the API-key field a no-op.
let editMenu = NSMenu(title: "Edit")
editMenu.addItem(withTitle: "Undo", action: Selector(("undo:")), keyEquivalent: "z")
editMenu.addItem(withTitle: "Redo", action: Selector(("redo:")), keyEquivalent: "Z")
editMenu.addItem(.separator())
editMenu.addItem(withTitle: "Cut", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
editMenu.addItem(withTitle: "Copy", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
editMenu.addItem(withTitle: "Paste", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
editMenu.addItem(withTitle: "Select All", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
let editMenuItem = NSMenuItem()
editMenuItem.submenu = editMenu
let mainMenu = NSMenu()
mainMenu.addItem(editMenuItem)
app.mainMenu = mainMenu

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

// A pipeline problem the user can fix (bad API key) replaces the live area in
// red until the next live render clears it.
func renderError(_ message: String) {
    liveLabel.attributedStringValue = NSAttributedString(string: message, attributes: [
        .foregroundColor: NSColor.systemRed,
        .font: mainFont, .paragraphStyle: mainPara,
    ])
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

// --- API key management ---
// The pipeline owns the key (storage and API calls); this app is just the UI.

// Sampled at launch: the button only appears when the variable is genuinely
// present (and non-blank) in the environment this process inherited.
let envKey = (ProcessInfo.processInfo.environment["OPENAI_API_KEY"] ?? "")
    .trimmingCharacters(in: .whitespacesAndNewlines)

func sendToPipeline(_ obj: [String: Any]) {
    guard let data = try? JSONSerialization.data(withJSONObject: obj) else { return }
    FileHandle.standardOutput.write(data)
    FileHandle.standardOutput.write(Data([0x0a]))
}

final class LinkOpener: NSObject {
    @objc func openKeysPage(_: Any?) {
        NSWorkspace.shared.open(URL(string: "https://platform.openai.com/account/api-keys")!)
    }
}
let linkOpener = LinkOpener()

func promptForKey(firstBoot: Bool) {
    let alert = NSAlert()
    alert.messageText = firstBoot ? "OpenAI API Key" : "Edit OpenAI API Key"
    alert.informativeText = firstBoot
        ? "Transcription and translation use the OpenAI API. Paste an API key to get started; it is stored locally for future runs."
        : "The new key replaces the stored one."
    // A wrapping, monospaced field: API keys are ~160 characters, and a
    // single-line field scrolls to show only the tail after a paste — which
    // reads as "the paste didn't work". Here the whole key is visible.
    let field = NSTextField(frame: NSRect(x: 0, y: 28, width: 420, height: 54))
    field.placeholderString = "sk-…"
    field.font = NSFont.monospacedSystemFont(ofSize: 11, weight: .regular)
    field.usesSingleLineMode = false
    field.lineBreakMode = .byCharWrapping
    field.cell?.wraps = true
    field.cell?.isScrollable = false
    let linkPara = NSMutableParagraphStyle()
    linkPara.alignment = .left
    let link = NSButton(title: "", target: linkOpener, action: #selector(LinkOpener.openKeysPage(_:)))
    link.isBordered = false
    link.attributedTitle = NSAttributedString(
        string: "You can find your API key at platform.openai.com/account/api-keys",
        attributes: [
            .foregroundColor: NSColor.linkColor,
            .font: NSFont.systemFont(ofSize: 11),
            .underlineStyle: NSUnderlineStyle.single.rawValue,
            .paragraphStyle: linkPara,
        ])
    link.frame = NSRect(x: -6, y: 2, width: 424, height: 18)
    let accessory = NSView(frame: NSRect(x: 0, y: 0, width: 420, height: 82))
    accessory.addSubview(field)
    accessory.addSubview(link)
    alert.accessoryView = accessory
    alert.window.initialFirstResponder = field
    alert.addButton(withTitle: "Save")
    if !envKey.isEmpty {
        alert.addButton(withTitle: "Use $OPENAI_API_KEY")
    }
    alert.addButton(withTitle: firstBoot ? "Quit" : "Cancel")
    NSApp.activate(ignoringOtherApps: true)
    while true {
        switch alert.runModal() {
        case .alertFirstButtonReturn: // Save
            // Keys never contain whitespace; drop any line breaks a wrapped
            // terminal copy smuggled in.
            let key = field.stringValue.components(separatedBy: .whitespacesAndNewlines).joined()
            if key.isEmpty {
                if firstBoot { continue } // can't do anything without one
                return
            }
            sendToPipeline(["type": "key", "key": key])
            return
        case .alertSecondButtonReturn where !envKey.isEmpty:
            sendToPipeline(["type": "key", "key": envKey])
            return
        default: // Quit / Cancel
            if firstBoot { app.terminate(nil) }
            return
        }
    }
}

// --- menu-bar settings ---

final class MenuActions: NSObject {
    @objc func showSettings(_ sender: Any?) {
        guard let view = sender as? NSView else { return }
        statusMenu.popUp(positioning: nil, at: NSPoint(x: 0, y: -4), in: view)
    }

    @objc func editKey(_: Any?) { promptForKey(firstBoot: false) }
    @objc func clearKeyAndQuit(_: Any?) {
        sendToPipeline(["type": "clear_key"])
        // The pipeline clears the stored key and shuts everything down, which
        // EOFs our stdin; the delayed terminate is a fallback if it's gone.
        DispatchQueue.main.asyncAfter(deadline: .now() + 2) { app.terminate(nil) }
    }
    @objc func quit(_: Any?) { app.terminate(nil) }
}
let menuActions = MenuActions()

let statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
statusItem.button?.image = NSImage(systemSymbolName: "captions.bubble",
                                   accessibilityDescription: "System Audio Subtitles")
let statusMenu = NSMenu()
let editItem = NSMenuItem(title: "Edit API Key…", action: #selector(MenuActions.editKey(_:)), keyEquivalent: "")
editItem.target = menuActions
statusMenu.addItem(editItem)
let clearItem = NSMenuItem(title: "Clear API Key & Quit", action: #selector(MenuActions.clearKeyAndQuit(_:)), keyEquivalent: "")
clearItem.target = menuActions
statusMenu.addItem(clearItem)
statusMenu.addItem(.separator())
let quitItem = NSMenuItem(title: "Quit", action: #selector(MenuActions.quit(_:)), keyEquivalent: "")
quitItem.target = menuActions
statusMenu.addItem(quitItem)
statusItem.menu = statusMenu

// The same menu is reachable from a gear in the window's top-left bar.
let settingsButton = NSButton()
settingsButton.isBordered = false
settingsButton.image = NSImage(systemSymbolName: "gearshape.fill", accessibilityDescription: "Settings")
settingsButton.symbolConfiguration = NSImage.SymbolConfiguration(pointSize: 12, weight: .regular)
settingsButton.contentTintColor = NSColor.white.withAlphaComponent(0.55)
settingsButton.target = menuActions
settingsButton.action = #selector(MenuActions.showSettings(_:))
settingsButton.translatesAutoresizingMaskIntoConstraints = false
bar.addSubview(settingsButton)
NSLayoutConstraint.activate([
    settingsButton.leadingAnchor.constraint(equalTo: closeButton.trailingAnchor, constant: 10),
    settingsButton.centerYAnchor.constraint(equalTo: bar.centerYAnchor),
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
            case "need_key":
                promptForKey(firstBoot: true)
            case "error":
                renderError(msg.message ?? "error")
            default:
                break
            }
        }
    }
    DispatchQueue.main.async { app.terminate(nil) } // pipeline exited
}

app.run()
