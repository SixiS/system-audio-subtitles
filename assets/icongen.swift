import AppKit

let S: CGFloat = 1024
let srgb = CGColorSpace(name: CGColorSpace.sRGB)!

func c(_ hex: UInt32, _ a: CGFloat = 1) -> CGColor {
    CGColor(srgbRed: CGFloat((hex >> 16) & 0xff) / 255,
            green: CGFloat((hex >> 8) & 0xff) / 255,
            blue: CGFloat(hex & 0xff) / 255, alpha: a)
}

func fillLinear(_ ctx: CGContext, _ colors: [CGColor], from: CGPoint, to: CGPoint) {
    let g = CGGradient(colorsSpace: srgb, colors: colors as CFArray, locations: nil)!
    ctx.drawLinearGradient(g, start: from, end: to,
                           options: [.drawsBeforeStartLocation, .drawsAfterEndLocation])
}

let rainbow: [UInt32] = [0xC0392B, 0xE67E22, 0xF5C33B, 0x4E9A51, 0x3E6FA8]

func makeIcon(tag: Bool, _ out: String) {
    let ctx = CGContext(data: nil, width: Int(S), height: Int(S), bitsPerComponent: 8,
                        bytesPerRow: 0, space: srgb,
                        bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue)!
    let full = CGRect(x: 0, y: 0, width: S, height: S)
    let body = CGPath(roundedRect: full, cornerWidth: 210, cornerHeight: 210, transform: nil)
    ctx.addPath(body)
    ctx.clip()

    // dark brown leather frame fills the whole icon
    fillLinear(ctx, [c(0x7C4E30), c(0x452412)], from: CGPoint(x: 0, y: S), to: CGPoint(x: 0, y: 0))
    ctx.setFillColor(c(0xFFFFFF, 0.14)) // sheen along the very top edge
    ctx.fill(CGRect(x: 0, y: 1004, width: S, height: 20))

    // creamy bezel ring
    let bezelRect = full.insetBy(dx: 64, dy: 64)
    let bezelPath = CGPath(roundedRect: bezelRect, cornerWidth: 150, cornerHeight: 150, transform: nil)
    ctx.saveGState()
    ctx.addPath(bezelPath)
    ctx.clip()
    fillLinear(ctx, [c(0xF4EFE2), c(0xCEC3AB)], from: CGPoint(x: 0, y: bezelRect.maxY), to: CGPoint(x: 0, y: bezelRect.minY))
    ctx.restoreGState()
    // seam where leather meets cream
    ctx.addPath(bezelPath)
    ctx.setStrokeColor(c(0x2E1809, 0.75))
    ctx.setLineWidth(5)
    ctx.strokePath()

    // dark glassy screen fills the middle
    let screenRect = full.insetBy(dx: 128, dy: 128)
    let screenPath = CGPath(roundedRect: screenRect, cornerWidth: 92, cornerHeight: 92, transform: nil)
    func paintScreen() {
        fillLinear(ctx, [c(0x46586E), c(0x0D131C)],
                   from: CGPoint(x: 0, y: screenRect.maxY), to: CGPoint(x: 0, y: screenRect.minY))
    }
    ctx.saveGState()
    ctx.addPath(screenPath)
    ctx.clip()
    paintScreen()

    // glossy white speech bubble
    let bubbleRect = CGRect(x: 232, y: 400, width: 560, height: 310)
    let bubble = CGMutablePath()
    bubble.addPath(CGPath(roundedRect: bubbleRect, cornerWidth: 96, cornerHeight: 96, transform: nil))
    let tail = CGMutablePath()
    tail.move(to: CGPoint(x: 350, y: 420))
    tail.addCurve(to: CGPoint(x: 302, y: 296),
                  control1: CGPoint(x: 352, y: 368), control2: CGPoint(x: 328, y: 324))
    tail.addCurve(to: CGPoint(x: 500, y: 412),
                  control1: CGPoint(x: 380, y: 330), control2: CGPoint(x: 444, y: 372))
    tail.closeSubpath()

    ctx.saveGState()
    ctx.setShadow(offset: CGSize(width: 0, height: -14), blur: 30, color: c(0x000000, 0.5))
    ctx.beginTransparencyLayer(auxiliaryInfo: nil)
    ctx.setFillColor(c(0xFFFFFF))
    ctx.addPath(bubble)
    ctx.fillPath()
    ctx.addPath(tail)
    ctx.fillPath()
    ctx.endTransparencyLayer()
    ctx.restoreGState()

    // gentle shading so the bubble reads as glossy plastic
    ctx.saveGState()
    ctx.addPath(bubble)
    ctx.addPath(tail)
    ctx.clip()
    fillLinear(ctx, [c(0xFFFFFF, 0), c(0xC6CCD8, 0.9)],
               from: CGPoint(x: 0, y: bubbleRect.maxY), to: CGPoint(x: 0, y: 296))
    ctx.restoreGState()

    // five rainbow dots
    let dotY: CGFloat = bubbleRect.midY
    let dotR: CGFloat = 34
    for (i, col) in rainbow.enumerated() {
        let x = 512 + (CGFloat(i) - 2) * 94
        ctx.setFillColor(c(col))
        ctx.fillEllipse(in: CGRect(x: x - dotR, y: dotY - dotR, width: dotR * 2, height: dotR * 2))
        ctx.setFillColor(c(0xFFFFFF, 0.4)) // tiny specular highlight
        ctx.fillEllipse(in: CGRect(x: x - dotR * 0.55, y: dotY + dotR * 0.1, width: dotR * 0.8, height: dotR * 0.55))
    }

    // angled glass glare across the screen, over everything on it
    let glare = CGMutablePath()
    glare.move(to: CGPoint(x: screenRect.minX, y: screenRect.maxY))
    glare.addLine(to: CGPoint(x: screenRect.minX + 470, y: screenRect.maxY))
    glare.addLine(to: CGPoint(x: screenRect.minX + 140, y: screenRect.minY))
    glare.addLine(to: CGPoint(x: screenRect.minX, y: screenRect.minY))
    glare.closeSubpath()
    ctx.addPath(glare)
    ctx.saveGState()
    ctx.clip()
    fillLinear(ctx, [c(0xFFFFFF, 0.24), c(0xFFFFFF, 0.03)],
               from: CGPoint(x: screenRect.minX, y: screenRect.maxY),
               to: CGPoint(x: screenRect.minX + 400, y: screenRect.minY))
    ctx.restoreGState()
    let streak = CGMutablePath()
    streak.move(to: CGPoint(x: screenRect.minX + 520, y: screenRect.maxY))
    streak.addLine(to: CGPoint(x: screenRect.minX + 595, y: screenRect.maxY))
    streak.addLine(to: CGPoint(x: screenRect.minX + 265, y: screenRect.minY))
    streak.addLine(to: CGPoint(x: screenRect.minX + 190, y: screenRect.minY))
    streak.closeSubpath()
    ctx.addPath(streak)
    ctx.setFillColor(c(0xFFFFFF, 0.12))
    ctx.fillPath()
    ctx.restoreGState() // screen clip

    // optional rainbow tag hanging over the top left
    if tag {
        ctx.saveGState()
        ctx.setShadow(offset: CGSize(width: 0, height: -8), blur: 16, color: c(0x000000, 0.4))
        ctx.beginTransparencyLayer(auxiliaryInfo: nil)
        for (i, col) in rainbow.enumerated() {
            ctx.setFillColor(c(col))
            ctx.fill(CGRect(x: 118 + CGFloat(i) * 40, y: 700, width: 40, height: S - 700))
        }
        ctx.endTransparencyLayer()
        ctx.restoreGState()
    }

    // subtle dark edge around the whole icon
    ctx.addPath(body)
    ctx.setStrokeColor(c(0x000000, 0.2))
    ctx.setLineWidth(6)
    ctx.strokePath()

    let rep = NSBitmapImageRep(cgImage: ctx.makeImage()!)
    let png = rep.representation(using: .png, properties: [:])!
    try! png.write(to: URL(fileURLWithPath: out))
    print("wrote \(out)")
}

let dir = CommandLine.arguments.count > 1 ? CommandLine.arguments[1] : "."
makeIcon(tag: false, dir + "/icon-leather.png")
makeIcon(tag: true, dir + "/icon-leather-tag.png")
