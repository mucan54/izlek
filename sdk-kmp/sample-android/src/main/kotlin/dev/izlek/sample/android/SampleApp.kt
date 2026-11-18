package dev.izlek.sample.android

import android.app.Activity
import android.app.Application
import android.os.Bundle
import android.text.InputType
import android.widget.Button
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.TextView
import dev.izlek.analytics.Analytics
import dev.izlek.analytics.Config
import dev.izlek.analytics.PlatformContext
import dev.izlek.analytics.flags.Flags
import dev.izlek.analytics.replay.AndroidViewTreeSource
import dev.izlek.analytics.replay.Replay
import dev.izlek.analytics.replay.ReplayConfig
import dev.izlek.analytics.replay.analyticsMask

class SampleApp : Application() {
    override fun onCreate() {
        super.onCreate()
        // 10.0.2.2 is the host machine as seen from the Android emulator.
        Analytics.setup(Config(token = "pk_dev", host = "http://10.0.2.2:8080"), PlatformContext(this))
        Flags.setup()
        Replay.setup(ReplayConfig(sampleRate = 1.0), AndroidViewTreeSource(this))
    }
}

class MainActivity : Activity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val title = TextView(this).apply { text = "izlek sample" }
        val email = EditText(this).apply { hint = "e-mail" } // input: masked in replay by default
        val password = EditText(this).apply {
            inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_PASSWORD
        }
        val balance = TextView(this).apply {
            text = "1.234,56 TL"
            analyticsMask() // explicit masking of a plain text
        }
        val button = Button(this).apply {
            text = if (Flags.isEnabled("new_checkout")) "Yeni ödeme" else "Devam"
            setOnClickListener {
                Analytics.capture("button_clicked", mapOf("screen" to "Main", "count" to 1))
                Analytics.identify("user-" + email.text.hashCode(), mapOf("plan" to "free"))
            }
        }
        setContentView(
            LinearLayout(this).apply {
                orientation = LinearLayout.VERTICAL
                for (v in listOf(title, email, password, balance, button)) addView(v)
            },
        )
        Analytics.screen("Main")
        Replay.screen("Main")
    }
}
