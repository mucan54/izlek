package dev.izlek.sample

import dev.izlek.analytics.Analytics
import dev.izlek.analytics.Config
import dev.izlek.analytics.PlatformContext
import dev.izlek.analytics.flags.Flags
import dev.izlek.analytics.flags.FlagsConfig
import kotlinx.coroutines.runBlocking
import java.nio.file.Files
import kotlin.system.exitProcess

private const val USAGE = """izlek sample-jvm

  --host <url>        ingestion base URL (required), e.g. http://localhost:8080
  --token <pk_...>    write token (required)
  --events <n>        number of "sample_event" events to send (default 3)
  --event <name>      event name (default sample_event)
  --user <id>         identify as this user before sending
  --reset             call reset() at the end (logout)
  --flag <key>        evaluate this feature flag (downloads <flags-host>/flags/<token>.json)
  --flags-host <url>  where the ruleset is served (default: --host)
  --dir <path>        storage directory (default: a fresh temporary directory)
  --no-gzip           send uncompressed bodies
  --no-lifecycle      do not send app_installed / app_opened

Exit code 0 when the last batch was accepted with HTTP 200, 1 otherwise."""

fun main(args: Array<String>) {
    val opts = HashMap<String, String>()
    var i = 0
    while (i < args.size) {
        val a = args[i]
        when (a) {
            "--reset", "--no-gzip", "--no-lifecycle" -> opts[a] = "true"
            "--help", "-h" -> {
                println(USAGE)
                return
            }
            else -> {
                if (!a.startsWith("--") || i + 1 >= args.size) {
                    System.err.println("unexpected argument: $a\n\n$USAGE")
                    exitProcess(2)
                }
                opts[a] = args[++i]
            }
        }
        i++
    }
    val host = opts["--host"]
    val token = opts["--token"]
    if (host == null || token == null) {
        System.err.println(USAGE)
        exitProcess(2)
    }
    val dir = opts["--dir"] ?: Files.createTempDirectory("izlek-sample").toString()
    val events = opts["--events"]?.toIntOrNull() ?: 3
    val eventName = opts["--event"] ?: "sample_event"

    val status = runBlocking {
        Analytics.setup(
            Config(
                token = token,
                host = host,
                gzip = opts["--no-gzip"] == null,
                captureLifecycle = opts["--no-lifecycle"] == null,
            ),
            PlatformContext(storageDir = dir, appVersion = "0.1.0", appBuild = "1"),
        )
        Analytics.register(mapOf("source" to "sample-jvm"))
        opts["--user"]?.let { Analytics.identify(it, mapOf("plan" to "sample")) }

        opts["--flag"]?.let { key ->
            Flags.setup(FlagsConfig(flagsHost = opts["--flags-host"]))
            val fresh = Flags.refresh()
            println("flag $key = ${Flags.flag(key)} (ruleset ${if (fresh) "downloaded" else "not available"})")
        }

        Analytics.screen("SampleScreen")
        repeat(events) { n ->
            Analytics.capture(eventName, mapOf("index" to n, "even" to (n % 2 == 0), "ratio" to n / 2.0, "tags" to listOf("a", "b")))
        }
        if (opts["--reset"] != null) Analytics.reset()
        Analytics.flush()

        val stats = Analytics.coreOrNull()?.stats
        println("storage: $dir")
        println("batches accepted: ${stats?.sentBatches}, records accepted: ${stats?.sentRecords}, last status: ${stats?.lastStatus}, dropped: ${stats?.droppedRecords}")
        stats?.lastStatus ?: -1
    }
    Analytics.shutdown()
    exitProcess(if (status == 200) 0 else 1)
}
