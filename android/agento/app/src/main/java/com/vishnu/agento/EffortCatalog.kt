package com.vishnu.agento

/**
 * Reasoning-effort options per model.
 *
 * The gateway validates `model_options.reasoning_effort` per request against
 * the levels the APPLIED model advertises, and applies the requested model
 * first — so this table is keyed on the model the turn will actually run,
 * and a level it offers that the model lacks fails loudly (400 naming the
 * real levels) instead of running silently at the wrong depth. Unknown values
 * are refused, not ignored.
 *
 * Measured 2026-10-04: mimo-v2.6-flash advertises off/minimal/low/medium/high
 * (graded — the old toggle claim came from Hermes-era files that no longer
 * exist). Sibling mimo rows follow the same shape but were NOT live-probed;
 * if one differs, the server's 400 names its real levels and the row gets
 * corrected then. The app sends "none" for off (the server aliases it).
 * Unknown models fall back to [FALLBACK].
 */
object EffortCatalog {

    /** Server default when no override is sent (matches gateway default). */
    const val DEFAULT = "medium"

    /** Full-ladder fallback for models with no specific entry. "ultra" is
     * deliberately absent: no model recognises it, so offering it would 400
     * every turn on an unknown model. */
    val FALLBACK: List<String> = listOf(
        "none", "minimal", "low", "medium", "high", "xhigh", "max",
    )

    /** Toggle-only families (reasoning off/on): "none" = off, "high" = on. */
    private val TOGGLE: List<String> = listOf("none", "high")

    /** MiMo graded ladder, as live-probed on v2.6-flash (#251). */
    private val MIMO_GRADED: List<String> = listOf(
        "none", "minimal", "low", "medium", "high",
    )

    /**
     * Normalized model key (lowercase, dots↔dashes unified, provider prefix
     * stripped) → offered effort values. First match wins on prefix.
     */
    private val TABLE: List<Pair<String, List<String>>> = listOf(
        // MiMo Flash: graded, not a toggle (live probe, #251). Pro/Omni/v2.5
        // follow the same shape unprobed — see the object KDoc.
        "mimo-v2-6-flash" to MIMO_GRADED,
        "mimo-v2-5" to MIMO_GRADED,
        "mimo-v2-6-pro" to MIMO_GRADED,
        "mimo-v2-pro" to MIMO_GRADED,
        "mimo-v2-omni" to MIMO_GRADED,
        // Muse Spark 1.x: graded levels.
        "muse-spark" to listOf("minimal", "low", "medium", "high", "xhigh", "max"),
        // GLM: 5/5.1 toggle; 5.2/5.3 graded.
        "glm-5-3" to listOf("low", "high", "max"),
        "glm-5-2" to listOf("low", "medium", "high", "xhigh", "max"),
        "glm-5-1" to TOGGLE,
        "glm-5-turbo" to TOGGLE,
        "glm-5" to TOGGLE,
        // DeepSeek V4 family.
        "deepseek-v4-1-flash" to listOf("none", "low", "high", "xhigh", "max"),
        "deepseek-v4-flash-vision" to listOf("none", "low", "high", "max"),
        "deepseek-v4-flash" to listOf("low", "medium", "high", "xhigh"),
        "deepseek-v4-pro" to listOf("low", "medium", "high", "xhigh"),
        "deepseek-flash" to listOf("low", "medium", "high", "xhigh"),
        // Qwen3: plus tiers toggle; max tiers graded.
        "qwen3-7" to listOf("none", "low", "medium", "high", "max"),
        "qwen3-8" to listOf("none", "low", "medium", "high", "max"),
        "qwen3-6-plus" to TOGGLE,
        "qwen3-5-plus" to TOGGLE,
        // Kimi K2/K3: toggle.
        "kimi-k2" to TOGGLE,
        "kimi-k3" to TOGGLE,
        // Grok 4.x: graded.
        "grok-4" to listOf("low", "medium", "high", "xhigh"),
    )

    /** Normalizes a model name for catalog lookup. */
    fun normalize(model: String): String {
        var m = model.trim().lowercase().replace('_', '-').replace('.', '-')
        // Strip a provider prefix ("xiaomi/mimo-..." or "opencode-go/...").
        val slash = m.lastIndexOf('/')
        if (slash >= 0) m = m.substring(slash + 1)
        // Strip a provider prefix ("xiaomi/mimo-..." or similar); family
        // matching below is prefix-based so channel suffixes
        // ("-free", "-contributor") need no special handling.
        return m
    }

    /** Effort values offered for [model]; [FALLBACK] when unknown/blank. */
    fun optionsFor(model: String): List<String> {
        val key = normalize(model)
        if (key.isEmpty()) return FALLBACK
        for ((prefix, opts) in TABLE) {
            if (key == prefix || key.startsWith(prefix + "-")) {
                return opts
            }
        }
        return FALLBACK
    }

    /** Sensible initial pick for [model]: "medium" when offered, else "high"
     * when offered (toggle families default to on), else the first option. */
    fun defaultFor(model: String): String {
        val opts = optionsFor(model)
        if (DEFAULT in opts) return DEFAULT
        if ("high" in opts) return "high"
        return opts.firstOrNull() ?: DEFAULT
    }

    /** Display label for an effort value ("none" reads as off). */
    fun labelFor(effort: String): String =
        if (effort.trim().lowercase() == "none") "none (off)" else effort
}
