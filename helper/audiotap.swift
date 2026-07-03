import AVFoundation
import CoreAudio
import Foundation

// audiotap: capture macOS system audio with a Core Audio process tap
// (macOS 14.4+) and stream 24 kHz mono s16le PCM to stdout.
// stderr carries diagnostics only; stdout is exclusively PCM bytes.
//
// Exit codes:
//   1  — Core Audio failure
//   2  — tap creation failed (usually: System Audio Recording permission denied)
//   64 — usage error (stdout is a terminal)

let outputSampleRate = 24000.0 // realtime API native rate; REST endpoints accept it too
let outputChannels: AVAudioChannelCount = 1

func note(_ message: String) {
    FileHandle.standardError.write(Data(("audiotap: " + message + "\n").utf8))
}

func fail(_ code: Int32, _ message: String) -> Never {
    note(message)
    exit(code)
}

func defaultOutputDeviceUID() -> String? {
    var address = AudioObjectPropertyAddress(
        mSelector: kAudioHardwarePropertyDefaultOutputDevice,
        mScope: kAudioObjectPropertyScopeGlobal,
        mElement: kAudioObjectPropertyElementMain)
    var deviceID = AudioObjectID(kAudioObjectUnknown)
    var size = UInt32(MemoryLayout<AudioObjectID>.size)
    guard AudioObjectGetPropertyData(AudioObjectID(kAudioObjectSystemObject),
                                     &address, 0, nil, &size, &deviceID) == noErr,
          deviceID != AudioObjectID(kAudioObjectUnknown) else { return nil }

    address.mSelector = kAudioDevicePropertyDeviceUID
    var uid: CFString = "" as CFString
    size = UInt32(MemoryLayout<CFString>.size)
    let status = withUnsafeMutablePointer(to: &uid) {
        AudioObjectGetPropertyData(deviceID, &address, 0, nil, &size, $0)
    }
    guard status == noErr else { return nil }
    return uid as String
}

if isatty(1) == 1 {
    fail(64, "stdout is a terminal; output is raw PCM — redirect it (audiotap > out.pcm)")
}
signal(SIGPIPE, SIG_IGN) // pipe closure is detected via write() returning EPIPE

// TCC attributes a CLI's permission request to the terminal app that
// (transitively) launched it, and refuses to even prompt when that app lacks
// NSAudioCaptureUsageDescription. Re-exec ourselves with *disclaimed
// responsibility* so this binary — whose embedded Info.plist has the key —
// becomes its own TCC subject and the System Audio Recording prompt can appear.
let disclaimMarker = "AUDIOTAP_DISCLAIMED"
if ProcessInfo.processInfo.environment[disclaimMarker] == nil {
    var attrs: posix_spawnattr_t?
    if posix_spawnattr_init(&attrs) == 0,
       let handle = dlopen(nil, RTLD_NOW),
       let symbol = dlsym(handle, "responsibility_spawnattrs_setdisclaim") {
        typealias SetDisclaimFn = @convention(c) (UnsafeMutablePointer<posix_spawnattr_t?>?, Int32) -> Int32
        let setDisclaim = unsafeBitCast(symbol, to: SetDisclaimFn.self)
        _ = setDisclaim(&attrs, 1)
        posix_spawnattr_setflags(&attrs, Int16(POSIX_SPAWN_SETEXEC))

        var pathBuffer = [CChar](repeating: 0, count: 4096)
        var pathLength = UInt32(pathBuffer.count)
        if _NSGetExecutablePath(&pathBuffer, &pathLength) == 0 {
            let execPath = String(cString: pathBuffer)
            setenv(disclaimMarker, "1", 1)
            var argv: [UnsafeMutablePointer<CChar>?] = CommandLine.arguments.map { strdup($0) }
            argv.append(nil)
            // With SETEXEC this replaces the current process and never returns
            // on success.
            let rc = posix_spawn(nil, execPath, nil, &attrs, argv, environ)
            note("re-exec with disclaimed responsibility failed (rc \(rc)); " +
                 "the permission prompt may not be able to appear")
        }
        posix_spawnattr_destroy(&attrs)
    }
}

// --- Global process tap: a stereo mixdown of every process's audio output.
let tapDescription = CATapDescription(stereoGlobalTapButExcludeProcesses: [])
tapDescription.name = "audiotap"
tapDescription.isPrivate = true
tapDescription.muteBehavior = .unmuted

var tapID = AudioObjectID(kAudioObjectUnknown)
var status = AudioHardwareCreateProcessTap(tapDescription, &tapID)
if status != noErr || tapID == AudioObjectID(kAudioObjectUnknown) {
    fail(2, "creating the system audio tap failed (OSStatus \(status)). " +
            "Likely cause: missing System Audio Recording permission. Grant it to your " +
            "terminal in System Settings → Privacy & Security → Screen & System Audio Recording, then rerun.")
}

// --- The tap's native format (typically 48 kHz stereo float32).
var formatAddress = AudioObjectPropertyAddress(
    mSelector: kAudioTapPropertyFormat,
    mScope: kAudioObjectPropertyScopeGlobal,
    mElement: kAudioObjectPropertyElementMain)
var tapASBD = AudioStreamBasicDescription()
var tapASBDSize = UInt32(MemoryLayout<AudioStreamBasicDescription>.size)
status = AudioObjectGetPropertyData(tapID, &formatAddress, 0, nil, &tapASBDSize, &tapASBD)
if status != noErr { fail(1, "reading tap format failed (OSStatus \(status))") }
guard let sourceFormat = AVAudioFormat(streamDescription: &tapASBD) else {
    fail(1, "tap format not representable as AVAudioFormat")
}

// --- Private aggregate device: default output device + the tap. The tap shows
// up as an input stream on the aggregate, which drives our IO proc at the
// output device's cadence.
// Known limitation (see PLAN.md): if the default output device changes while
// running, capture sticks to the old device until restart.
guard let outputUID = defaultOutputDeviceUID() else {
    fail(1, "could not determine the default output device")
}
let aggregateDescription: [String: Any] = [
    kAudioAggregateDeviceNameKey as String: "audiotap-aggregate",
    kAudioAggregateDeviceUIDKey as String: "audiotap-" + UUID().uuidString,
    kAudioAggregateDeviceMainSubDeviceKey as String: outputUID,
    kAudioAggregateDeviceIsPrivateKey as String: true,
    kAudioAggregateDeviceIsStackedKey as String: false,
    kAudioAggregateDeviceTapAutoStartKey as String: true,
    kAudioAggregateDeviceSubDeviceListKey as String: [
        [kAudioSubDeviceUIDKey as String: outputUID]
    ],
    kAudioAggregateDeviceTapListKey as String: [
        [kAudioSubTapUIDKey as String: tapDescription.uuid.uuidString,
         kAudioSubTapDriftCompensationKey as String: true]
    ],
]
var aggregateID = AudioObjectID(kAudioObjectUnknown)
status = AudioHardwareCreateAggregateDevice(aggregateDescription as CFDictionary, &aggregateID)
if status != noErr { fail(1, "creating aggregate device failed (OSStatus \(status))") }

// --- Converter: tap native format → 16 kHz mono s16le.
guard let outputFormat = AVAudioFormat(commonFormat: .pcmFormatInt16,
                                       sampleRate: outputSampleRate,
                                       channels: outputChannels,
                                       interleaved: true),
      let converter = AVAudioConverter(from: sourceFormat, to: outputFormat) else {
    fail(1, "could not build a \(Int(sourceFormat.sampleRate)) Hz → \(Int(outputSampleRate)) Hz converter")
}

let writeQueue = DispatchQueue(label: "audiotap.write")
var warnedAboutSilence = false
var silentFramesObserved: UInt64 = 0

// stdout writes happen off the Core Audio IO thread so a stalled reader can
// never glitch capture.
func emit(_ data: Data) {
    writeQueue.async {
        data.withUnsafeBytes { (raw: UnsafeRawBufferPointer) in
            var offset = 0
            while offset < raw.count {
                let n = write(1, raw.baseAddress! + offset, raw.count - offset)
                if n <= 0 { exit(0) } // reader closed the pipe; we are done
                offset += n
            }
        }
    }
}

func convertAndEmit(_ inputBuffer: AVAudioPCMBuffer) {
    let ratio = outputSampleRate / sourceFormat.sampleRate
    let capacity = AVAudioFrameCount(Double(inputBuffer.frameLength) * ratio) + 64
    guard let outputBuffer = AVAudioPCMBuffer(pcmFormat: outputFormat, frameCapacity: capacity) else { return }

    var handedOff = false
    var convertError: NSError?
    converter.convert(to: outputBuffer, error: &convertError) { _, outStatus in
        if handedOff {
            outStatus.pointee = .noDataNow
            return nil
        }
        handedOff = true
        outStatus.pointee = .haveData
        return inputBuffer
    }
    if convertError != nil || outputBuffer.frameLength == 0 { return }
    guard let samples = outputBuffer.int16ChannelData else { return }

    let sampleCount = Int(outputBuffer.frameLength) * Int(outputChannels)
    if !warnedAboutSilence {
        var allZero = true
        for i in 0..<sampleCount where samples[0][i] != 0 {
            allZero = false
            break
        }
        if allZero {
            silentFramesObserved += UInt64(outputBuffer.frameLength)
            if silentFramesObserved > UInt64(outputSampleRate * 3) {
                note("3 s captured, every sample is zero — is audio playing? " +
                     "(If yes, check the System Audio Recording permission.)")
                warnedAboutSilence = true
            }
        } else {
            silentFramesObserved = 0
        }
    }

    emit(Data(bytes: samples[0], count: sampleCount * MemoryLayout<Int16>.size))
}

var ioProcID: AudioDeviceIOProcID?
status = AudioDeviceCreateIOProcIDWithBlock(&ioProcID, aggregateID, nil) { _, inInputData, _, _, _ in
    guard let inputBuffer = AVAudioPCMBuffer(pcmFormat: sourceFormat,
                                             bufferListNoCopy: inInputData,
                                             deallocator: nil),
          inputBuffer.frameLength > 0 else { return }
    convertAndEmit(inputBuffer)
}
if status != noErr || ioProcID == nil { fail(1, "installing IO proc failed (OSStatus \(status))") }

status = AudioDeviceStart(aggregateID, ioProcID)
if status != noErr { fail(1, "starting capture failed (OSStatus \(status))") }

note("capturing system audio: \(Int(sourceFormat.sampleRate)) Hz " +
     "\(sourceFormat.channelCount)ch → \(Int(outputSampleRate)) Hz mono s16le on stdout")

var signalSources: [DispatchSourceSignal] = []
for sig in [SIGINT, SIGTERM] {
    signal(sig, SIG_IGN)
    let source = DispatchSource.makeSignalSource(signal: sig, queue: writeQueue)
    source.setEventHandler {
        AudioDeviceStop(aggregateID, ioProcID)
        if let ioProcID { AudioDeviceDestroyIOProcID(aggregateID, ioProcID) }
        AudioHardwareDestroyAggregateDevice(aggregateID)
        AudioHardwareDestroyProcessTap(tapID)
        exit(0)
    }
    source.resume()
    signalSources.append(source)
}

dispatchMain()
