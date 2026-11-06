@file:OptIn(ExperimentalMaterial3Api::class)

package com.vishnu.agento

/**
 * ∃-Mail inbox screen (#365): the user's read-only inbox of agent-filed
 * reports. Newest first; tap opens (and marks read, driving the drawer
 * badge); star toggles; delete confirms and hard-deletes. No compose, no
 * reply, no search, no folders — v1 interactions only.
 *
 * Math is compiled on the server: `text` segments render with the shared
 * mikepenz Markdown, `math` segments show pre-rendered HTML in a WebView
 * styled by the vendored KaTeX CSS (`assets/katex`, display-only). The app
 * never compiles LaTeX.
 */
import android.annotation.SuppressLint
import android.webkit.WebView
import android.webkit.WebViewClient
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.ArrowBack
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Mail
import androidx.compose.material.icons.filled.Menu
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.Star
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalDensity
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.viewinterop.AndroidView
import com.mikepenz.markdown.m3.Markdown
import kotlinx.coroutines.launch

@Composable
internal fun ReportsScreen(
    onMenu: () -> Unit = {},
    onUnreadChanged: (Int) -> Unit = {},
) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val toasts = remember(scope) { ToastCenter(scope) }
    var reports by remember { mutableStateOf<List<ReportSummary>>(emptyList()) }
    var error by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var loaded by remember { mutableStateOf(false) }
    var openId by remember { mutableStateOf<String?>(null) }
    var deleting by remember { mutableStateOf<ReportSummary?>(null) }

    fun load(silent: Boolean = false) {
        if (!silent) { busy = true; error = "" }
        scope.launch {
            ReportsApi(context).list().fold(
                onSuccess = {
                    reports = it
                    loaded = true
                    onUnreadChanged(it.count { r -> !r.read })
                },
                onFailure = { e ->
                    error = e.message ?: e.javaClass.simpleName
                    loaded = true
                },
            )
            busy = false
        }
    }

    fun mutate(work: suspend () -> Result<Unit>, okToast: String) {
        scope.launch {
            work().fold(
                onSuccess = {
                    load(silent = true)
                    toasts.toast(okToast, ToastKind.Success)
                },
                onFailure = { e ->
                    toasts.toast(friendlyError(e.message ?: "").title, ToastKind.Error)
                },
            )
        }
    }

    LaunchedEffect(Unit) { load() }

    // Detail view is in-screen state (not a destination): the back button
    // returns to the list without touching navigation.
    val open = openId
    if (open != null) {
        ReportDetailView(
            reportId = open,
            onBack = {
                openId = null
                load(silent = true)
            },
            onDeleted = {
                openId = null
                load(silent = true)
            },
            onChanged = { load(silent = true) },
        )
        return
    }

    Box(modifier = Modifier.fillMaxSize()) {
    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("∃-Mail") },
                navigationIcon = {
                    IconButton(onClick = onMenu) {
                        Icon(Icons.Filled.Menu, contentDescription = "Menu")
                    }
                },
                actions = {
                    IconButton(onClick = { load() }, enabled = !busy) {
                        Icon(Icons.Filled.Refresh, contentDescription = "Refresh")
                    }
                },
            )
        },
    ) { padding ->
        Column(modifier = Modifier.fillMaxSize().padding(padding)) {
            if (busy) LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
            if (error.isNotEmpty() && reports.isEmpty()) {
                ErrorCard(raw = error, onRetry = { load() },
                    modifier = Modifier.padding(12.dp))
            }
            if (loaded && reports.isEmpty() && error.isEmpty()) {
                EmptyState(
                    icon = Icons.Filled.Mail,
                    title = "No reports yet",
                    subtitle = "Agents file markdown reports with LaTeX math here. Newest first.",
                )
            }
            LazyColumn(
                modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp),
                verticalArrangement = Arrangement.spacedBy(8.dp),
                contentPadding = PaddingValues(vertical = 8.dp),
            ) {
                items(reports, key = { it.id }) { report ->
                    Card(
                        onClick = { openId = report.id },
                        modifier = Modifier.fillMaxWidth(),
                    ) {
                        Column(modifier = Modifier.fillMaxWidth().padding(12.dp)) {
                            Row(verticalAlignment = Alignment.CenterVertically) {
                                Column(modifier = Modifier.weight(1f)) {
                                    Text(
                                        report.title.ifEmpty { "(untitled report)" },
                                        style = MaterialTheme.typography.bodyLarge.copy(
                                            fontWeight = if (!report.read) FontWeight.Bold else null,
                                        ),
                                        maxLines = 2,
                                        overflow = TextOverflow.Ellipsis,
                                    )
                                    val meta = formatReportTime(report.createdAt)
                                    if (meta.isNotEmpty()) {
                                        Text(
                                            meta,
                                            style = MaterialTheme.typography.labelSmall,
                                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                                        )
                                    }
                                }
                                IconButton(onClick = {
                                    mutate(
                                        { ReportsApi(context).patch(report.id, null, !report.starred) },
                                        if (!report.starred) "Report starred." else "Report unstarred.",
                                    )
                                }) {
                                    Icon(
                                        Icons.Filled.Star,
                                        contentDescription = if (report.starred) "Unstar" else "Star",
                                        tint = if (report.starred) {
                                            MaterialTheme.colorScheme.primary
                                        } else {
                                            MaterialTheme.colorScheme.onSurfaceVariant
                                        },
                                    )
                                }
                                IconButton(onClick = { deleting = report }) {
                                    Icon(Icons.Filled.Delete, contentDescription = "Delete report")
                                }
                            }
                            if (report.preview.isNotEmpty()) {
                                Text(
                                    report.preview,
                                    style = MaterialTheme.typography.bodySmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    maxLines = 2,
                                    overflow = TextOverflow.Ellipsis,
                                )
                            }
                        }
                    }
                }
            }
            ToastHost(center = toasts, modifier = Modifier.fillMaxWidth())
        }
    }
    }

    val deleteTarget = deleting
    if (deleteTarget != null) {
        AlertDialog(
            onDismissRequest = { deleting = null },
            title = { Text("Delete report?") },
            text = { Text("“${deleteTarget.title.ifEmpty { "Untitled report" }}” will be permanently deleted. This can't be undone.") },
            confirmButton = {
                TextButton(onClick = {
                    deleting = null
                    mutate(
                        { ReportsApi(context).delete(deleteTarget.id) },
                        "Report deleted.",
                    )
                }) { Text("Delete") }
            },
            dismissButton = {
                TextButton(onClick = { deleting = null }) { Text("Cancel") }
            },
        )
    }
}

/**
 * One open report: full body as server-rendered segments. Text renders with
 * mikepenz Markdown; math shows the pre-rendered HTML in a local-asset
 * WebView (no network, no JavaScript — the HTML is pre-compiled).
 *
 * Opening marks read: the GET is followed by a silent PATCH when the report
 * arrives unread, which is what drives the drawer badge down.
 */
@Composable
private fun ReportDetailView(
    reportId: String,
    onBack: () -> Unit,
    onDeleted: () -> Unit,
    onChanged: () -> Unit,
) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val toasts = remember(scope) { ToastCenter(scope) }
    var detail by remember(reportId) { mutableStateOf<ReportDetail?>(null) }
    var error by remember(reportId) { mutableStateOf("") }
    var busy by remember(reportId) { mutableStateOf(true) }
    var confirmingDelete by remember { mutableStateOf(false) }

    fun markReadIfNeeded(d: ReportDetail) {
        if (d.read) return
        scope.launch {
            ReportsApi(context).patch(d.id, true, null).fold(
                onSuccess = {
                    detail = d.copy(read = true)
                    onChanged()
                },
                onFailure = { },
            )
        }
    }

    LaunchedEffect(reportId) {
        busy = true
        error = ""
        ReportsApi(context).get(reportId).fold(
            onSuccess = { d ->
                detail = d
                markReadIfNeeded(d)
            },
            onFailure = { e -> error = e.message ?: e.javaClass.simpleName },
        )
        busy = false
    }

    Box(modifier = Modifier.fillMaxSize()) {
    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text(detail?.title?.ifEmpty { "Report" } ?: "Report", maxLines = 1) },
                navigationIcon = {
                    IconButton(onClick = onBack) {
                        Icon(Icons.Filled.ArrowBack, contentDescription = "Back")
                    }
                },
                actions = {
                    val d = detail
                    if (d != null) {
                        IconButton(onClick = {
                            scope.launch {
                                ReportsApi(context).patch(d.id, null, !d.starred).fold(
                                    onSuccess = {
                                        detail = d.copy(starred = !d.starred)
                                        onChanged()
                                        toasts.toast(
                                            if (!d.starred) "Report starred." else "Report unstarred.",
                                            ToastKind.Success,
                                        )
                                    },
                                    onFailure = { e ->
                                        toasts.toast(friendlyError(e.message ?: "").title, ToastKind.Error)
                                    },
                                )
                            }
                        }) {
                            Icon(
                                Icons.Filled.Star,
                                contentDescription = if (d.starred) "Unstar" else "Star",
                                tint = if (d.starred) {
                                    MaterialTheme.colorScheme.primary
                                } else {
                                    MaterialTheme.colorScheme.onSurfaceVariant
                                },
                            )
                        }
                        IconButton(onClick = { confirmingDelete = true }) {
                            Icon(Icons.Filled.Delete, contentDescription = "Delete report")
                        }
                    }
                },
            )
        },
    ) { padding ->
        Column(modifier = Modifier.fillMaxSize().padding(padding)) {
            if (busy) LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
            if (error.isNotEmpty() && detail == null) {
                ErrorCard(raw = error, onRetry = null, modifier = Modifier.padding(12.dp))
            }
            val d = detail
            if (d != null) {
                val meta = listOfNotNull(
                    formatReportTime(d.createdAt).takeIf { it.isNotEmpty() },
                    d.source.takeIf { it.isNotEmpty() },
                ).joinToString(" · ")
                Column(
                    modifier = Modifier.fillMaxSize().verticalScroll(rememberScrollState())
                        .padding(horizontal = 12.dp, vertical = 8.dp),
                    verticalArrangement = Arrangement.spacedBy(8.dp),
                ) {
                    if (meta.isNotEmpty()) {
                        Text(
                            meta,
                            style = MaterialTheme.typography.labelSmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                    ReportBody(segments = d.segments, bodyMd = d.bodyMd)
                }
            }
            ToastHost(center = toasts, modifier = Modifier.fillMaxWidth())
        }
    }
    }

    if (confirmingDelete) {
        AlertDialog(
            onDismissRequest = { confirmingDelete = false },
            title = { Text("Delete report?") },
            text = { Text("This report will be permanently deleted. This can't be undone.") },
            confirmButton = {
                TextButton(onClick = {
                    confirmingDelete = false
                    scope.launch {
                        ReportsApi(context).delete(reportId).fold(
                            onSuccess = {
                                toasts.toast("Report deleted.", ToastKind.Success)
                                onDeleted()
                            },
                            onFailure = { e ->
                                toasts.toast(friendlyError(e.message ?: "").title, ToastKind.Error)
                            },
                        )
                    }
                }) { Text("Delete") }
            },
            dismissButton = {
                TextButton(onClick = { confirmingDelete = false }) { Text("Cancel") }
            },
        )
    }
}

/** Body segments: markdown text natively, pre-rendered math in a WebView. */
@Composable
private fun ReportBody(segments: List<ReportSegment>, bodyMd: String) {
    if (segments.isEmpty()) {
        // No segments (older server or empty body): raw markdown is the
        // honest fallback, never a blank screen.
        if (bodyMd.isNotEmpty()) {
            SelectionContainer {
                Markdown(content = bodyMd, modifier = Modifier.fillMaxWidth())
            }
        }
        return
    }
    segments.forEach { seg ->
        when (seg.kind) {
            "text" -> {
                if (seg.text.isNotEmpty()) {
                    SelectionContainer {
                        Markdown(content = seg.text, modifier = Modifier.fillMaxWidth())
                    }
                }
            }
            "math" -> {
                if (seg.html.isNotEmpty()) {
                    MathView(html = seg.html, display = seg.display)
                }
            }
        }
    }
}

/**
 * One math fragment in a local-asset WebView. JavaScript stays off (the HTML
 * is pre-compiled server-side); the base URL resolves `katex.min.css` and
 * its fonts from the APK, so rendering is fully offline. Height follows the
 * laid-out content via contentHeight — fragments are capped server-side, so
 * the post-load jump is one small reflow, not a layout loop.
 */
@SuppressLint("SetJavaScriptEnabled")
@Composable
private fun MathView(html: String, display: Boolean) {
    val fg = MaterialTheme.colorScheme.onSurface
    val fgHex = "#%02X%02X%02X".format(
        (fg.red * 255).toInt(), (fg.green * 255).toInt(), (fg.blue * 255).toInt(),
    )
    val fontPx = with(LocalDensity.current) { 16.sp.toPx() }
    val doc = remember(html, fgHex, fontPx, display) {
        """<html><head><meta name="viewport" content="width=device-width, initial-scale=1">""" +
            """<link rel="stylesheet" href="katex.min.css"></head>""" +
            """<body style="margin:0;padding:0;background-color:transparent;color:$fgHex">""" +
            """<div style="font-size:${fontPx}px;overflow-x:auto;""" +
            (if (display) "padding:4px 0;" else "display:inline-block;") + """"$>$html</div></body></html>"""
    }
    var loadedDoc by remember { mutableStateOf<String?>(null) }
    AndroidView(
        factory = { ctx ->
            WebView(ctx).apply {
                setBackgroundColor(android.graphics.Color.TRANSPARENT)
                isVerticalScrollBarEnabled = false
                isHorizontalScrollBarEnabled = false
                webViewClient = object : WebViewClient() {
                    override fun onPageFinished(view: WebView, url: String?) {
                        view.post {
                            val h = (view.contentHeight * view.resources.displayMetrics.density).toInt()
                            if (h > 0 && view.layoutParams?.height != h) {
                                view.layoutParams = view.layoutParams?.apply { height = h }
                            }
                        }
                    }
                }
            }
        },
        update = { view ->
            if (loadedDoc != doc) {
                loadedDoc = doc
                view.loadDataWithBaseURL("file:///android_asset/katex/", doc, "text/html", "utf-8", null)
            }
        },
        modifier = Modifier.fillMaxWidth().heightIn(min = 24.dp),
    )
}
