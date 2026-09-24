package com.vishnu.agento

import android.os.Build
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Check
import androidx.compose.material.icons.filled.ContentCopy
import androidx.compose.material.icons.filled.Info
import androidx.compose.material.icons.filled.Warning
import androidx.compose.material3.AssistChip
import androidx.compose.material3.AssistChipDefaults
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.dynamicDarkColorScheme
import androidx.compose.material3.dynamicLightColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp

/**
 * Shared design system for the Agento UI overhaul (#64).
 *
 * - [AgentoTheme]: branded Material3 theme (seeded green, dynamic color on
 *   Android 12+). Every screen renders inside it — no bare
 *   `lightColorScheme()` / `darkColorScheme()` calls elsewhere.
 * - [friendlyError]: maps raw technical failures (HTTP codes, IOExceptions,
 *   gateway messages) to a human title + next step. The raw text is never
 *   shown as the headline; [ErrorCard] tucks it behind Details.
 * - [EmptyState], [SectionCard], [StatusChip], [ErrorCard]: the component
 *   set every screen is built from, so spacing/typography stay consistent.
 */

// ---------------------------------------------------------------------------
// Theme
// ---------------------------------------------------------------------------

private val Seed = Color(0xFF1B5E20)

private val LightSeedScheme = lightColorScheme(
    primary = Color(0xFF1B5E20),
    onPrimary = Color.White,
    primaryContainer = Color(0xFFD9EDDA),
    onPrimaryContainer = Color(0xFF0B2E0E),
    secondary = Color(0xFF4C6444),
    secondaryContainer = Color(0xFFCDEBC0),
    tertiary = Color(0xFF38656A),
    error = Color(0xFFBA1A1A),
    surfaceVariant = Color(0xFFE7EFE4),
)

private val DarkSeedScheme = darkColorScheme(
    primary = Color(0xFF8FD694),
    onPrimary = Color(0xFF00390F),
    primaryContainer = Color(0xFF144A1E),
    onPrimaryContainer = Color(0xFFD9EDDA),
    secondary = Color(0xFFB1CBA5),
    secondaryContainer = Color(0xFF33492D),
    tertiary = Color(0xFF8CD0D4),
    error = Color(0xFFFFB4AB),
    surfaceVariant = Color(0xFF1E2B1F),
)

/** Branded app theme: dynamic color on Android 12+, seeded green otherwise. */
@Composable
fun AgentoTheme(dark: Boolean, content: @Composable () -> Unit) {
    val context = LocalContext.current
    val scheme = when {
        Build.VERSION.SDK_INT >= Build.VERSION_CODES.S -> {
            if (dark) dynamicDarkColorScheme(context) else dynamicLightColorScheme(context)
        }
        dark -> DarkSeedScheme
        else -> LightSeedScheme
    }
    MaterialTheme(colorScheme = scheme, content = content)
}

// ---------------------------------------------------------------------------
// Responsive helpers
// ---------------------------------------------------------------------------

/** Width buckets driving adaptive layouts (phones → tablets → landscape). */
enum class WindowClass { Compact, Medium, Expanded }

fun windowClassFor(maxWidth: Dp): WindowClass = when {
    maxWidth < 600.dp -> WindowClass.Compact
    maxWidth < 840.dp -> WindowClass.Medium
    else -> WindowClass.Expanded
}

/** Centers content with a readable max width on every screen size. */
fun Modifier.contentWidth(wc: WindowClass): Modifier = when (wc) {
    WindowClass.Compact -> this.fillMaxWidth()
    WindowClass.Medium -> this.fillMaxWidth().widthIn(max = 720.dp)
    WindowClass.Expanded -> this.fillMaxWidth().widthIn(max = 960.dp)
}

/** Chat bubble width as a fraction of the row, per window class. */
fun bubbleFraction(wc: WindowClass): Float = when (wc) {
    WindowClass.Compact -> 0.88f
    WindowClass.Medium -> 0.76f
    WindowClass.Expanded -> 0.64f
}

// ---------------------------------------------------------------------------
// Friendly errors — user-facing copy lives here, not inline in screens.
// ---------------------------------------------------------------------------

/** Human headline + next step for a raw technical failure. */
data class FriendlyError(val title: String, val nextStep: String)

fun friendlyError(raw: String): FriendlyError {
    val r = raw.trim()
    val low = r.lowercase()
    fun has(vararg needles: String) = needles.any { it in low }
    return when {
        r.isEmpty() -> FriendlyError(
            "Something went wrong",
            "Please try again.",
        )
        has("server url not configured", "server not configured", "not configured — see settings") -> FriendlyError(
            "Server not set up yet",
            "Open Settings → Server and enter the server URL and password.",
        )
        has("http 401", "http 403", "unauthorized", "forbidden", "invalid password", "wrong password") -> FriendlyError(
            "Server rejected the password",
            "Check the password in Settings → Server, save, and retry.",
        )
        has("http 404", "not found") -> FriendlyError(
            "Server didn't recognise that request",
            "The app may be outdated — check Settings → App updates, then retry.",
        )
        has("http 429", "rate limit", "too many requests") -> FriendlyError(
            "Server is busy",
            "Wait a moment and try again.",
        )
        has("http 500", "http 502", "http 503", "http 504", "internal error", "bad gateway", "unavailable") -> FriendlyError(
            "Server had a problem",
            "The request reached the server but it failed. Wait a bit and retry.",
        )
        has("http 4") && has("http") -> FriendlyError(
            "Request failed",
            "Check your settings and try again.",
        )
        has("failed to connect", "unable to resolve", "unknownhost", "connectexception",
            "socket", "network is unreachable", "no address", "eai_nodata") -> FriendlyError(
            "Can't reach the server",
            "Check the server URL, make sure you're on the right network, then retry.",
        )
        has("timeout", "timed out", "deadline exceeded") -> FriendlyError(
            "Server took too long",
            "The connection timed out. Check your network and retry.",
        )
        has("ssl", "certificate", "tls", "handshake") -> FriendlyError(
            "Secure connection failed",
            "Use http:// for local addresses or fix the server certificate, then retry.",
        )
        has("empty reply", "empty response") -> FriendlyError(
            "Assistant sent an empty reply",
            "Try asking again or pick a different model.",
        )
        has("permission", "not granted") -> FriendlyError(
            "Permission needed",
            "Grant the requested permission in the system dialog or app settings.",
        )
        has("no providers", "no apk asset", "no releases") -> FriendlyError(
            "Nothing available yet",
            "Check the server side, then reload and try again.",
        )
        else -> FriendlyError(
            "Something went wrong",
            "Please try again. Details below can help diagnose it.",
        )
    }
}

/** Status-line copy for transient confirmations (snackbars, never raw dumps). */
object Toasts {
    const val SAVED = "Saved."
    const val SERVER_SAVED = "Server saved."
    const val TASK_SAVED = "Task saved."
    const val TASK_DELETED = "Task deleted."
    const val EXPORTED = "Settings exported — keep the file somewhere safe."
    const val DOWNLOADING = "Downloading — check the notification shade."
    const val COPIED = "Copied."
    const val TEST_SENT = "Test sent — check the notification shade."
    const val UP_TO_DATE = "You're up to date."
}

// ---------------------------------------------------------------------------
// Components
// ---------------------------------------------------------------------------

/** Friendly empty state: icon + headline + explainer + optional action. */
@Composable
fun EmptyState(
    icon: ImageVector,
    title: String,
    subtitle: String,
    actionLabel: String? = null,
    onAction: (() -> Unit)? = null,
    modifier: Modifier = Modifier,
) {
    Column(
        modifier = modifier.fillMaxWidth().padding(24.dp),
        horizontalAlignment = Alignment.CenterHorizontally,
        verticalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        Card(
            shape = RoundedCornerShape(20.dp),
            colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.secondaryContainer),
        ) {
            Icon(
                icon,
                contentDescription = null,
                tint = MaterialTheme.colorScheme.onSecondaryContainer,
                modifier = Modifier.padding(16.dp).size(32.dp),
            )
        }
        Text(title, style = MaterialTheme.typography.titleMedium)
        Text(
            subtitle,
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        if (actionLabel != null && onAction != null) {
            Spacer(modifier = Modifier.height(4.dp))
            TextButton(onClick = onAction) { Text(actionLabel) }
        }
    }
}

/** Titled card grouping one settings section (title + explainer + content). */
@Composable
fun SectionCard(
    title: String,
    subtitle: String = "",
    modifier: Modifier = Modifier,
    content: @Composable () -> Unit,
) {
    Card(modifier = modifier.fillMaxWidth()) {
        Column(
            modifier = Modifier.fillMaxWidth().padding(16.dp),
            verticalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            Text(title, style = MaterialTheme.typography.titleSmall)
            if (subtitle.isNotEmpty()) {
                Text(
                    subtitle,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            content()
        }
    }
}

/** Colored status chip (tasks); unknown values fall back to neutral Todo. */
@Composable
fun StatusChip(status: String, onClick: (() -> Unit)? = null, modifier: Modifier = Modifier) {
    val (container, content) = when (status) {
        "Ongoing" -> MaterialTheme.colorScheme.primaryContainer to MaterialTheme.colorScheme.onPrimaryContainer
        "Paused" -> Color(0xFFFFE3B3) to Color(0xFF4A2C00)
        "Done" -> Color(0xFFCDEBC0) to Color(0xFF0B2E0E)
        else -> MaterialTheme.colorScheme.surfaceVariant to MaterialTheme.colorScheme.onSurfaceVariant
    }
    val chip: @Composable () -> Unit = {
        AssistChip(
            onClick = { onClick?.invoke() },
            enabled = onClick != null,
            label = { Text(status) },
            leadingIcon = {
                if (status == "Done") {
                    Icon(Icons.Filled.Check, contentDescription = null)
                }
            },
            colors = AssistChipDefaults.assistChipColors(containerColor = container, labelColor = content),
            modifier = modifier,
        )
    }
    chip()
}

/** Inline error: friendly headline + next step + expandable technical details. */
@Composable
fun ErrorCard(
    raw: String,
    onRetry: (() -> Unit)? = null,
    modifier: Modifier = Modifier,
) {
    if (raw.isBlank()) return
    val friendly = remember(raw) { friendlyError(raw) }
    var expanded by remember(raw) { mutableStateOf(false) }
    val clipboard = LocalClipboardManager.current
    Card(
        modifier = modifier.fillMaxWidth(),
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.errorContainer),
    ) {
        Column(modifier = Modifier.fillMaxWidth().padding(12.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Icon(
                    Icons.Filled.Warning,
                    contentDescription = null,
                    tint = MaterialTheme.colorScheme.onErrorContainer,
                )
                Spacer(modifier = Modifier.width(8.dp))
                Text(
                    friendly.title,
                    style = MaterialTheme.typography.titleSmall,
                    color = MaterialTheme.colorScheme.onErrorContainer,
                    modifier = Modifier.weight(1f),
                )
                if (onRetry != null) {
                    TextButton(onClick = onRetry) { Text("Retry") }
                }
            }
            Text(
                friendly.nextStep,
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onErrorContainer,
            )
            Row(verticalAlignment = Alignment.CenterVertically) {
                TextButton(onClick = { expanded = !expanded }) {
                    Text(if (expanded) "Hide details" else "Details")
                }
                Spacer(modifier = Modifier.weight(1f))
                IconButton(onClick = { clipboard.setText(AnnotatedString(raw)) }) {
                    Icon(
                        Icons.Filled.ContentCopy,
                        contentDescription = "Copy error details",
                        tint = MaterialTheme.colorScheme.onErrorContainer,
                    )
                }
            }
            if (expanded) {
                Text(
                    raw.take(600),
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onErrorContainer,
                )
            }
        }
    }
}

/** Small status line with an info icon (setup hints, sync state, versions). */
@Composable
fun HintLine(text: String, modifier: Modifier = Modifier) {
    Row(modifier = modifier, verticalAlignment = Alignment.CenterVertically) {
        Icon(
            Icons.Filled.Info,
            contentDescription = null,
            tint = MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier.size(16.dp),
        )
        Spacer(modifier = Modifier.width(6.dp))
        Text(
            text,
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}
