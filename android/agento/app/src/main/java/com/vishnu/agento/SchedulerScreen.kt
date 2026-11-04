@file:OptIn(ExperimentalMaterial3Api::class)

package com.vishnu.agento

/**
 * Gateway scheduler UI and the job dialog (#163, last of four moves).
 *
 * Moved verbatim out of MainActivity.kt; the only changes are
 * visibility (`private` to `internal`, same package) and the import
 * list, which carries exactly what this file uses.
 */
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.grid.items
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Edit
import androidx.compose.material.icons.filled.List
import androidx.compose.material.icons.filled.Menu
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.Schedule
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.LifecycleResumeEffect
import kotlinx.coroutines.Job
import kotlinx.coroutines.launch
/** Gateway scheduler: native cron jobs over the Jobs API (same auth as
 * chat, through the proxy's /p/ route — no server changes needed).
 * Each job is a stored cron chat: one prompt, one response per fire, short
 * history per job (#289). */
@Composable
internal fun SchedulerScreen(
    onMenu: () -> Unit = {},
    deepLinkJobId: String? = null,
    onDeepLinkConsumed: () -> Unit = {},
) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val toasts = remember(scope) { ToastCenter(scope) }
    var jobs by remember { mutableStateOf<List<CronJob>>(emptyList()) }
    // Unparseable rows the server sent but the list skipped (#430): counted
    // in the parse, shown below, never a silent shrink.
    var dropped by remember { mutableIntStateOf(0) }
    var error by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var loaded by remember { mutableStateOf(false) }
    var editing by remember { mutableStateOf<CronJob?>(null) }
    var creating by remember { mutableStateOf(false) }
    var deleting by remember { mutableStateOf<CronJob?>(null) }
    // Stored cron chat: the tapped job's prompt plus its short history.
    var detail by remember { mutableStateOf<CronJob?>(null) }

    fun path(): String = ChatApi(context).pathFor("chat")

    fun load(silent: Boolean = false) {
        if (!silent) { busy = true; error = "" }
        scope.launch {
            JobsApi(context).list(path()).fold(
                onSuccess = {
                    jobs = it.jobs
                    dropped = it.dropped
                    // A successful load retires the failure (#447 review),
                    // silent or not: leaving it would pin the error card,
                    // block the empty state, and freeze the detail dialog on
                    // a ghost row.
                    error = ""
                    loaded = true
                    // Same discovery as the background worker, over the
                    // rows just fetched rather than a second fetch: a
                    // response that landed while away notifies on open.
                    CronCheck.notifyFresh(context, it.jobs, it.dropped)
                },
                onFailure = { e ->
                    val msg = e.message ?: e.javaClass.simpleName
                    error = msg
                    // A failed refresh over rows already on screen must say
                    // so (#447 review): without the toast the stale rows
                    // read as current. The dropped count stays: it describes
                    // the rows still showing (last success), not the failed
                    // fetch — same rule as the Task Manager list.
                    toasts.toast(friendlyError(msg).title, ToastKind.Error)
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

    // Silent refresh on return (#447 review): the hub, the picker and the
    // Task Manager list all re-read on resume, so a gateway outage between
    // backgrounding and foregrounding can't leave this list showing
    // pre-outage rows with no signal. Guarded like the Task Manager's: a
    // resume before the first load finishes does nothing — that load is
    // already fetching.
    LifecycleResumeEffect(Unit) {
        if (loaded) {
            val job = scope.launch { load(silent = true) }
            onPauseOrDispose { job.cancel() }
        } else {
            onPauseOrDispose { }
        }
    }

    // Notification deep-link into one job's stored chat: opened once the
    // list carries the row, then cleared upstream. `dropped` is a key:
    // without it a clean fetch returning structurally-equal jobs wouldn't
    // re-run this effect, and a pending id would never resolve after the
    // parse recovered (#447 review). Toasts fire once per id: without the
    // guard every refresh re-announces the same pending tap.
    var deepLinkWarned by remember { mutableStateOf<String?>(null) }
    LaunchedEffect(jobs, loaded, deepLinkJobId, dropped) {
        val id = deepLinkJobId ?: return@LaunchedEffect
        val found = jobs.firstOrNull { it.id == id }
        if (found != null) {
            detail = found
            deepLinkWarned = null
            onDeepLinkConsumed()
        } else if (loaded && dropped == 0) {
            // The row never arrived and the parse was clean — dropped,
            // deleted, or never existing (#447 review): say so instead of
            // ignoring the tap forever, and consume so a dead id can't pin
            // this effect.
            toasts.toast("That job couldn't be opened.", ToastKind.Error)
            deepLinkWarned = null
            onDeepLinkConsumed()
        } else if (loaded && deepLinkWarned != id) {
            // Unparseable rows are still outstanding (#447 review): the
            // missing id may belong to one of them. Stay pending for a clean
            // fetch instead of discarding the deep link.
            deepLinkWarned = id
            toasts.toast("That job hasn't loaded yet — will retry on the next refresh.", ToastKind.Info)
        }
    }

    // The toast card floats top-end over the content (Box overlay, not
    // the bottom-anchored Scaffold slot) so it never shifts the layout.
    Box(modifier = Modifier.fillMaxSize()) {
    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("Scheduler") },
                navigationIcon = {
                    IconButton(onClick = onMenu) {
                        Icon(Icons.Filled.Menu, contentDescription = "Menu")
                    }
                },
                actions = {
                    // No New action while any rows are unreadable (#447
                    // review): same suppression as the Task Manager/Projects
                    // lists — creating against a partially unreadable list
                    // risks duplicates of rows that failed to parse.
                    val unreadable = dropped > 0
                    IconButton(onClick = { creating = true }, enabled = !unreadable) {
                        Icon(Icons.Filled.Add, contentDescription = "New job")
                    }
                    IconButton(onClick = { load() }, enabled = !busy) {
                        Icon(Icons.Filled.Refresh, contentDescription = "Refresh")
                    }
                },
            )
        },
    ) { padding ->
        Column(modifier = Modifier.fillMaxSize().padding(padding)) {
            if (busy) LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
            if (error.isNotEmpty() && jobs.isEmpty()) {
                ErrorCard(raw = error, onRetry = { load() },
                    modifier = Modifier.padding(12.dp))
            }
            // Standalone hint only for partial lists (#447 review): when
            // everything was dropped the EmptyState below carries the same
            // message — showing both reads the count twice.
            if (dropped > 0 && jobs.isNotEmpty()) {
                HintLine("$dropped job(s) could not be parsed — the list below is partial.")
            }
            if (loaded && jobs.isEmpty() && error.isEmpty()) {
                // All-dropped reads as empty without this (#447 review):
                // same guard as the Task Manager list.
                if (dropped > 0) {
                    HintLine(
                        "$dropped job(s) could not be parsed — the list below would be incomplete.",
                        modifier = Modifier.padding(horizontal = 12.dp),
                    )
                }
                EmptyState(
                    icon = Icons.Filled.Schedule,
                    title = if (dropped > 0) "Couldn't read jobs" else "No scheduled jobs",
                    subtitle = if (dropped > 0) {
                        "The server sent rows this app can't read — check the gateway version."
                    } else {
                        "Jobs run prompts on a schedule, even when the app is closed."
                    },
                    actionLabel = if (dropped > 0) null else "New job",
                    onAction = if (dropped > 0) {
                        null
                    } else {
                        { creating = true }
                    },
                )
            }
            LazyColumn(
                modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp),
                verticalArrangement = Arrangement.spacedBy(8.dp),
                contentPadding = PaddingValues(vertical = 8.dp),
            ) {
                items(jobs, key = { it.id }) { job ->
                    var menu by remember(job.id) { mutableStateOf(false) }
                    Card(
                        onClick = { detail = job },
                        modifier = Modifier.fillMaxWidth(),
                    ) {
                        Column(modifier = Modifier.fillMaxWidth().padding(12.dp)) {
                            Row(verticalAlignment = Alignment.CenterVertically) {
                                Column(modifier = Modifier.weight(1f)) {
                                    Text(
                                        job.name.ifEmpty { "(unnamed job)" },
                                        style = MaterialTheme.typography.bodyLarge,
                                        maxLines = 1,
                                    )
                                    if (job.schedule.isNotEmpty()) {
                                        Text(
                                            job.schedule,
                                            style = MaterialTheme.typography.bodySmall,
                                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                                        )
                                    }
                                }
                                val paused = job.paused
                                AssistChip(
                                    onClick = {
                                        if (paused == true) {
                                            mutate({ JobsApi(context).resume(path(), job.id) }, "Job resumed.")
                                        } else {
                                            mutate({ JobsApi(context).pause(path(), job.id) }, "Job paused.")
                                        }
                                    },
                                    label = { Text(when (paused) { true -> "Paused"; false -> "Active"; null -> "—" }) },
                                )
                                Box {
                                    IconButton(onClick = { menu = true }) {
                                        Icon(Icons.Filled.MoreVert, contentDescription = "Job menu")
                                    }
                                    DropdownMenu(expanded = menu, onDismissRequest = { menu = false }) {
                                        DropdownMenuItem(
                                            text = { Text("Run now") },
                                            onClick = {
                                                menu = false
                                                mutate({ JobsApi(context).runNow(path(), job.id) }, "Job started.")
                                            },
                                        )
                                        DropdownMenuItem(
                                            text = { Text("Edit") },
                                            onClick = { menu = false; editing = job },
                                        )
                                        DropdownMenuItem(
                                            text = { Text("Delete") },
                                            onClick = { menu = false; deleting = job },
                                        )
                                    }
                                }
                            }
                            if (job.prompt.isNotEmpty()) {
                                Text(
                                    job.prompt,
                                    style = MaterialTheme.typography.bodySmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    maxLines = 3,
                                )
                            }
                            val meta = listOfNotNull(
                                job.nextRun.takeIf { it.isNotEmpty() }?.let { "Next: $it" },
                                job.lastRun.takeIf { it.isNotEmpty() }?.let { "Last: $it" },
                                job.delivery.takeIf { it.isNotEmpty() }?.let { "To: $it" },
                            ).joinToString(" · ")
                            if (meta.isNotEmpty()) {
                                Text(
                                    meta,
                                    style = MaterialTheme.typography.labelSmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                )
                            }
                            // Latest stored response, so the row reads as
                            // the cron chat it is — tap opens the history.
                            job.runs.lastOrNull { it.text.isNotEmpty() }?.let { last ->
                                Text(
                                    (if (last.missed) "Missed: " else "") + last.text,
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
        }
        ToastHost(center = toasts, modifier = Modifier.fillMaxWidth())
    }
    }

    if (creating) {
        JobDialog(
            initial = null,
            onDismiss = { creating = false },
            onSave = { name, prompt, schedule, delivery ->
                creating = false
                mutate(
                    { JobsApi(context).create(path(), name, prompt, schedule, delivery) },
                    "Job created.",
                )
            },
        )
    }
    val editTarget = editing
    if (editTarget != null) {
        JobDialog(
            initial = editTarget,
            onDismiss = { editing = null },
            onSave = { name, prompt, schedule, delivery ->
                editing = null
                mutate(
                    { JobsApi(context).update(path(), editTarget.id, name, prompt, schedule, delivery) },
                    "Job updated.",
                )
            },
        )
    }
    val deleteTarget = deleting
    if (deleteTarget != null) {
        AlertDialog(
            onDismissRequest = { deleting = null },
            title = { Text("Delete job?") },
            text = { Text("“${deleteTarget.name.ifEmpty { deleteTarget.schedule.ifEmpty { "Unnamed job" } }}” will stop running. This can't be undone.") },
            confirmButton = {
                TextButton(onClick = {
                    deleting = null
                    mutate({ JobsApi(context).delete(path(), deleteTarget.id) }, "Job deleted.")
                }) { Text("Delete") }
            },
            dismissButton = {
                TextButton(onClick = { deleting = null }) { Text("Cancel") }
            },
        )
    }
    // Stored cron chat: the prompt plus the short run history, re-resolved
    // against the live list so a run that lands while open appears. A row
    // that left the latest successful list (deleted elsewhere, or newly
    // unparseable) closes instead of showing the stale snapshot (#447
    // review); a failed fetch keeps the dialog, since the rows may simply
    // be stale rather than gone.
    val detailJob = detail?.let { d -> jobs.firstOrNull { it.id == d.id } }
    if (detailJob != null) {
        AlertDialog(
            onDismissRequest = { detail = null },
            title = { Text(detailJob.name.ifEmpty { "Scheduled job" }) },
            text = {
                Column(
                    modifier = Modifier.verticalScroll(rememberScrollState()),
                    verticalArrangement = Arrangement.spacedBy(8.dp),
                ) {
                    Text(
                        "Schedule: ${detailJob.schedule.ifEmpty { "—" }}" +
                            (detailJob.nextRun.takeIf { it.isNotEmpty() }?.let { " · next $it" } ?: ""),
                        style = MaterialTheme.typography.labelMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    if (detailJob.prompt.isNotEmpty()) {
                        Text(detailJob.prompt, style = MaterialTheme.typography.bodyMedium)
                    }
                    val history = detailJob.runs.asReversed()
                    if (history.isEmpty()) {
                        HintLine("No responses yet — the next fire stores one here.")
                    } else {
                        history.forEach { run ->
                            Column(verticalArrangement = Arrangement.spacedBy(2.dp)) {
                                Text(
                                    (if (run.missed) "Missed" else "Responded") +
                                        (run.at.takeIf { it > 0 }?.let { " · ${formatCronRunAt(it)}" } ?: ""),
                                    style = MaterialTheme.typography.labelSmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                )
                                if (run.text.isNotEmpty()) {
                                    Text(run.text, style = MaterialTheme.typography.bodyMedium)
                                }
                            }
                        }
                    }
                }
            },
            confirmButton = {
                TextButton(onClick = { detail = null }) { Text("Close") }
            },
            dismissButton = {
                Row(horizontalArrangement = Arrangement.spacedBy(4.dp)) {
                    TextButton(onClick = {
                        mutate({ JobsApi(context).runNow(path(), detailJob.id) }, "Job started.")
                    }) { Text("Run now") }
                    TextButton(onClick = {
                        detail = null
                        editing = detailJob
                    }) { Text("Edit") }
                }
            },
        )
    } else if (detail != null && loaded && error.isEmpty()) {
        // The open row is gone from the latest successful list — deleted
        // elsewhere, or newly unparseable (#447 review). Close (acting on
        // the stale snapshot would misfire) and say so, once per row.
        LaunchedEffect(detail?.id) {
            toasts.toast("That job is no longer available.", ToastKind.Error)
            detail = null
        }
    }
}

/** "14 Oct, 09:00" for a run instant, IST-pinned like every displayed time. */
internal fun formatCronRunAt(at: Long): String =
    java.time.Instant.ofEpochMilli(at).atZone(IST)
        .format(java.time.format.DateTimeFormatter.ofPattern("d MMM, HH:mm", java.util.Locale.ENGLISH))

/** Create/edit dialog for one scheduled job. */
@Composable
internal fun JobDialog(
    initial: CronJob?,
    onDismiss: () -> Unit,
    onSave: (name: String, prompt: String, schedule: String, delivery: String) -> Unit,
) {
    var name by remember { mutableStateOf(initial?.name ?: "") }
    var prompt by remember { mutableStateOf(initial?.prompt ?: "") }
    var schedule by remember { mutableStateOf(initial?.schedule ?: "") }
    var delivery by remember { mutableStateOf(initial?.delivery ?: "local") }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(if (initial == null) "New job" else "Edit job") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                OutlinedTextField(
                    value = name,
                    onValueChange = { name = it },
                    label = { Text("Name (optional)") },
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth(),
                )
                OutlinedTextField(
                    value = prompt,
                    onValueChange = { prompt = it },
                    label = { Text("Prompt") },
                    modifier = Modifier.fillMaxWidth(),
                    maxLines = 4,
                )
                OutlinedTextField(
                    value = schedule,
                    onValueChange = { schedule = it },
                    label = { Text("Schedule (cron, e.g. 0 9 * * *)") },
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth(),
                )
                OutlinedTextField(
                    value = delivery,
                    onValueChange = { delivery = it },
                    label = { Text("Deliver to (local, telegram, …)") },
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth(),
                )
            }
        },
        confirmButton = {
            TextButton(
                onClick = { onSave(name.trim(), prompt.trim(), schedule.trim(), delivery.trim()) },
                enabled = prompt.isNotBlank() && schedule.isNotBlank(),
            ) { Text("Save") }
        },
        dismissButton = {
            TextButton(onClick = onDismiss) { Text("Cancel") }
        },
    )
}
