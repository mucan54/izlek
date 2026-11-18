import org.gradle.api.DefaultTask
import org.gradle.api.file.ConfigurableFileCollection
import org.gradle.api.file.DirectoryProperty
import org.gradle.api.provider.Property
import org.gradle.api.tasks.Input
import org.gradle.api.tasks.InputFiles
import org.gradle.api.tasks.OutputDirectory
import org.gradle.api.tasks.PathSensitive
import org.gradle.api.tasks.PathSensitivity
import org.gradle.api.tasks.TaskAction

/**
 * Embeds the JSON test vectors under `spec/vectors/` into a Kotlin source file so that
 * common (multiplatform) tests can read them without any file I/O.
 *
 * The JSON files stay the single source of truth: the generated file only contains their
 * text, split into chunks small enough for JVM class-file string constants.
 */
abstract class GenerateSpecVectorsTask : DefaultTask() {

    @get:InputFiles
    @get:PathSensitive(PathSensitivity.NAME_ONLY)
    abstract val vectorFiles: ConfigurableFileCollection

    @get:Input
    abstract val packageName: Property<String>

    @get:OutputDirectory
    abstract val outputDir: DirectoryProperty

    @TaskAction
    fun generate() {
        val pkg = packageName.get()
        val out = StringBuilder()
        out.append("// Generated from spec/vectors/*.json by GenerateSpecVectorsTask. Do not edit.\n")
        out.append("package ").append(pkg).append("\n\n")
        out.append("/** Raw JSON text of the shared test vectors (spec/vectors). */\n")
        out.append("public object SpecVectors {\n")
        val sorted = vectorFiles.files.sortedBy { it.name }
        for (f in sorted) {
            val name = f.name.removeSuffix(".json").replace(Regex("[^A-Za-z0-9]"), "_")
            val text = f.readText(Charsets.UTF_8)
            out.append("    /** ").append(f.name).append(" */\n")
            out.append("    public val ").append(name).append("Json: String get() = ").append(name)
                .append("Chunks.joinToString(\"\")\n\n")
            out.append("    private val ").append(name).append("Chunks: Array<String> = arrayOf(\n")
            var i = 0
            while (i < text.length) {
                val end = minOf(text.length, i + CHUNK)
                out.append("        \"")
                for (j in i until end) appendEscaped(out, text[j])
                out.append("\",\n")
                i = end
            }
            out.append("    )\n\n")
        }
        out.append("}\n")
        val dir = outputDir.get().asFile.resolve(pkg.replace('.', '/'))
        dir.mkdirs()
        dir.resolve("SpecVectors.kt").writeText(out.toString(), Charsets.UTF_8)
    }

    private fun appendEscaped(out: StringBuilder, c: Char) {
        when {
            c == '\\' -> out.append("\\\\")
            c == '"' -> out.append("\\\"")
            c == '$' -> out.append("\\$")
            c == '\n' -> out.append("\\n")
            c == '\r' -> out.append("\\r")
            c == '\t' -> out.append("\\t")
            c.code < 0x20 || c.code > 0x7E -> out.append("\\u").append(c.code.toString(16).padStart(4, '0'))
            else -> out.append(c)
        }
    }

    private companion object {
        const val CHUNK = 8000
    }
}
