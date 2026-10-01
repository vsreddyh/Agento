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
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.Job
import kotlinx.coroutines.launch
/** Gateway scheduler: native cron jobs over the Jobs API (same auth as
 * chat, through the proxy's /p/ route — no server changes needed). */
@Composable
internal fun SchedulerScreen(onMenu: () -> Unit = {}) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val snackbar = remember { SnackbarHostState() }
    var jobs by remember { mutableStateOf<List<CronJob>>(emptyList()) }
    var error by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var loaded by remember { mutableStateOf(false) }
    var editing by remember { mutableStateOf<CronJob?>(null) }
    var creating by remember { mutableStateOf(false) }
    var deleting by remember { mutableStateOf<CronJob?>(null) }

    fun path(): String = ChatApi(context).pathFor("god")

    fun load(silent: Boolean = false) {
        if (!silent) { busy = true; error = "" }
        scope.launch {
            JobsApi(context).list(path()).fold(
                onSuccess = { jobs = it; loaded = true },
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
                    snackbar.showSnackbar(okToast)
                },
                onFailure = { e ->
                    snackbar.showSnackbar(friendlyError(e.message ?: "").title)
                },
            )
        }
    }

    LaunchedEffect(Unit) { load() }

    Scaffold(
        snackbarHost = { SnackbarHost(snackbar) },
        topBar = {
            TopAppBar(
                title = { Text("Scheduler") },
                navigationIcon = {
                    IconButton(onClick = onMenu) {
                        Icon(Icons.Filled.Menu, contentDescription = "Menu")
                    }
                },
                actions = {
                    IconButton(onClick = { creating = true }) {
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
            if (loaded && jobs.isEmpty() && error.isEmpty()) {
                EmptyState(
                    icon = Icons.Filled.Schedule,
                    title = "No scheduled jobs",
                    subtitle = "Jobs run prompts on a schedule, even when the app is closed.",
                    actionLabel = "New job",
                    onAction = { creating = true },
                )
            }
            LazyColumn(
                modifier = Modifier.fillMaxWidth().padding(horizontal = 12.dp),
                verticalArrangement = Arrangement.spacedBy(8.dp),
                contentPadding = PaddingValues(vertical = 8.dp),
            ) {
                items(jobs, key = { it.id }) { job ->
                    var menu by remember(job.id) { mutableStateOf(false) }
                    Card(modifier = Modifier.fillMaxWidth()) {
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
                        }
                    }
                }
            }
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
}

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
