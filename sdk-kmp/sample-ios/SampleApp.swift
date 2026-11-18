// Unverified skeleton: never compiled (no Xcode on the machine this SDK was developed on).
import SwiftUI
import Shared // the application's KMP framework that exports the analytics-* modules

@main
struct SampleApp: App {
    init() {
        // Must run on the main thread (reads UIDevice / UIScreen once).
        Analytics.shared.setup(
            config: Config(
                token: "pk_dev",
                host: "http://localhost:8080",
                flushAt: 20,
                flushIntervalMs: 30_000,
                maxQueueBytes: 10 * 1024 * 1024,
                maxEventAgeMs: 7 * 24 * 3600 * 1000,
                captureLifecycle: true,
                gzip: true
            ),
            context: PlatformContext()
        )
        Flags.shared.setup(config: FlagsConfig(flagsHost: nil, refreshIntervalMs: 300_000))
        Replay.shared.setup(
            config: ReplayConfig(sampleRate: 1.0, maskAllText: false, frameIntervalMs: 500, maxSpoolBytes: 5 * 1024 * 1024),
            source: UIKitViewTreeSource()
        )
    }

    var body: some Scene {
        WindowGroup {
            Button("Devam") {
                Analytics.shared.capture(event: "button_clicked", props: ["screen": "Main"])
                if Flags.shared.isEnabled(key: "new_checkout", default: false) {
                    Analytics.shared.screen(name: "NewCheckout", props: [:])
                }
            }
        }
    }
}
