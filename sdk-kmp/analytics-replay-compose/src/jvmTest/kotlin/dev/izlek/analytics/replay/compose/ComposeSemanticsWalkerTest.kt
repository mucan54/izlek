package dev.izlek.analytics.replay.compose

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.BasicSecureTextField
import androidx.compose.foundation.text.BasicText
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.foundation.text.input.rememberTextFieldState
import androidx.compose.foundation.verticalScroll
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.compose.ui.ExperimentalComposeUiApi
import androidx.compose.ui.Modifier
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.role
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.test.ExperimentalTestApi
import androidx.compose.ui.test.onRoot
import androidx.compose.ui.test.runComposeUiTest
import androidx.compose.ui.unit.dp
import dev.izlek.analytics.replay.MaskMode
import dev.izlek.analytics.replay.Masking
import dev.izlek.analytics.replay.NodeFlags
import dev.izlek.analytics.replay.NodeType
import dev.izlek.analytics.replay.RawNode
import dev.izlek.analytics.replay.StableIds
import dev.izlek.analytics.replay.TreeDiffer
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertIs
import kotlin.test.assertNotNull
import kotlin.test.assertNull
import kotlin.test.assertTrue

/**
 * Prototype for design §19 assumption 7: what a real Compose semantics tree (Compose
 * Multiplatform on the desktop JVM, rendered off-screen) gives the replay walker.
 */
@OptIn(ExperimentalTestApi::class, ExperimentalComposeUiApi::class)
class ComposeSemanticsWalkerTest {

    @Test
    fun semanticsTreeBecomesReplayNodes() = runComposeUiTest {
        var title by mutableStateOf("Hoş geldin")
        setContent {
            Column(Modifier.verticalScroll(rememberScrollState())) {
                BasicText(title)
                BasicTextField(rememberTextFieldState("search term"), Modifier.width(200.dp))
                BasicSecureTextField(rememberTextFieldState("hunter2"), Modifier.width(200.dp))
                Box(Modifier.size(120.dp).semantics { role = Role.Image })
                Box(Modifier.size(200.dp, 48.dp).clickable(role = Role.Button) {}) { BasicText("Devam") }
                Column(Modifier.analyticsMask()) { BasicText("IBAN TR00 0000") }
                Box(Modifier.size(50.dp)) // no semantics: invisible to the walker
                Spacer(Modifier.height(10.dp))
            }
        }
        val ids = StableIds()
        fun capture(): List<RawNode> {
            val out = ArrayList<RawNode>()
            val root = onRoot(useUnmergedTree = true).fetchSemanticsNode()
            ComposeSemanticsWalker.walk(root, 0, root.layoutInfo.density.density, 0, 0, { ids.idFor(it) }, out)
            ids.endPass()
            return out
        }

        val raw = capture()
        val byText = raw.filter { it.text != null }.associateBy { it.text!! }

        val titleNode = assertNotNull(byText["Hoş geldin"])
        assertEquals(NodeType.TEXT, titleNode.type)
        assertTrue(titleNode.w > 0 && titleNode.h > 0)

        val input = assertNotNull(byText["search term"])
        assertEquals(NodeType.INPUT, input.type)
        assertEquals(200, input.w)
        assertEquals(false, input.secure)

        val password = raw.single { it.secure }
        assertEquals(NodeType.INPUT, password.type)

        assertEquals(1, raw.count { it.type == NodeType.IMAGE })
        val image = raw.single { it.type == NodeType.IMAGE }
        assertEquals(listOf(120, 120), listOf(image.w, image.h))

        val button = raw.single { it.type == NodeType.BUTTON }
        assertTrue(button.clickable)
        assertEquals(listOf(200, 48), listOf(button.w, button.h))
        assertEquals(button.id, assertNotNull(byText["Devam"]).parent, "the label is a child of the button")

        assertEquals(1, raw.count { it.type == NodeType.SCROLL })
        assertEquals(1, raw.count { it.mask == MaskMode.MASK })
        assertTrue(raw.all { it.color == null }, "semantics carries no colors")
        // parents always come first
        val seen = HashSet<Long>()
        for (n in raw) {
            assertTrue(n.parent == 0L || n.parent in seen, "parent of ${n.id} listed before it")
            seen.add(n.id)
        }
        // vertical layout: y grows down the column
        assertTrue(input.y > titleNode.y && button.y > image.y)

        // masking applied to what Compose reported
        val masked = Masking.apply(raw, maskAllText = false)
        val texts = masked.mapNotNull { it.text }
        assertTrue("Hoş geldin" in texts && "Devam" in texts)
        assertTrue("•".repeat("search term".length) in texts, "input fields are masked by default")
        assertTrue("•".repeat("IBAN TR00 0000".length) in texts, "Modifier.analyticsMask() masks the subtree")
        assertTrue(texts.none { it.contains("hunter2") || it.contains("IBAN") || it.contains("search") })
        assertNull(masked.single { it.type == NodeType.IMAGE }.text)
        assertTrue(masked.filter { it.flags and NodeFlags.MASKED != 0 }.size >= 3)

        // a recomposition shows up as a small delta with stable ids
        title = "Hoş geldin, Ayşe"
        waitForIdle()
        val after = Masking.apply(capture(), maskAllText = false)
        val delta = assertIs<TreeDiffer.Diff.Delta>(TreeDiffer.diff(masked, after))
        assertEquals(emptyList(), delta.removed)
        assertEquals(listOf("Hoş geldin, Ayşe"), delta.upserted.map { it.text })
        assertEquals(titleNode.id, delta.upserted.single().id)
    }
}
