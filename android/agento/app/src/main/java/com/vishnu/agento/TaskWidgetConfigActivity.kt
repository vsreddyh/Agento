package com.vishnu.agento

import android.appwidget.AppWidgetManager
import android.content.Context
import android.content.Intent
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.ArrowBack
import androidx.compose.material.icons.filled.RadioButtonUnchecked
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material3.*
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp

/**
 * Display settings for one task-widget placement: which slice it shows,
 * row density, the due line, and whether the list scrolls. Launched by
 * the system at add time (android:configure) and from the Task Manager
 * screen afterwards. Cancelling at add time aborts the placement;
 * saving re-renders that widget via [TaskWidget].
 */
class TaskWidgetConfigActivity : ComponentActivity() {

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val appWidgetId = intent?.extras?.getInt(
            AppWidgetManager.EXTRA_APPWIDGET_ID,
            AppWidgetManager.INVALID_APPWIDGET_ID,
        ) ?: AppWidgetManager.INVALID_APPWIDGET_ID
        // No id (shouldn't happen): nothing to configure.
        if (appWidgetId == AppWidgetManager.INVALID_APPWIDGET_ID) {
            setResult(RESULT_CANCELED)
            finish()
            return
        }
        // Return the id on save so the host binds this placement.
        val done = Intent().putExtra(AppWidgetManager.EXTRA_APPWIDGET_ID, appWidgetId)
        setResult(RESULT_CANCELED, done)
        setContent {
            val dark = isSystemInDarkTheme()
            AgentoTheme(dark = dark) {
                WidgetSettingsScreen(
                    view = TaskWidget.viewFor(this, appWidgetId),
                    density = TaskWidget.densityFor(this, appWidgetId),
                    showDue = TaskWidget.showDueFor(this, appWidgetId),
                    scrollable = TaskWidget.scrollableFor(this, appWidgetId),
                    onCancel = {
                        setResult(RESULT_CANCELED, done)
                        finish()
                    },
                    onSave = { v, d, due, scroll ->
                        prefs().edit()
                            .putString("task_widget_view_$appWidgetId", v.name)
                            .putString("task_widget_density_$appWidgetId", d.name)
                            .putBoolean("task_widget_due_$appWidgetId", due)
                            .putBoolean("task_widget_scroll_$appWidgetId", scroll)
                            // Retired diagnostic ladder: drop the key here
                            // too, not just on widget delete.
                            .remove("task_widget_style_$appWidgetId")
                            .apply()
                        TaskWidget.refresh(this@TaskWidgetConfigActivity)
                        setResult(RESULT_OK, done)
                        finish()
                    },
                )
            }
        }
    }

    private fun prefs() = applicationContext.getSharedPreferences(
        AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
}

/** Settings for one placement. Everything is live: the preview above the
 * controls redraws as you pick, so a choice never has to be imagined. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun WidgetSettingsScreen(
    view: TaskWidgetView,
    density: TaskWidgetDensity,
    showDue: Boolean,
    scrollable: Boolean,
    onCancel: () -> Unit,
    onSave: (TaskWidgetView, TaskWidgetDensity, Boolean, Boolean) -> Unit,
) {
    var pickedView by remember { mutableStateOf(view) }
    var pickedDensity by remember { mutableStateOf(density) }
    var pickedDue by remember { mutableStateOf(showDue) }
    var pickedScroll by remember { mutableStateOf(scrollable) }

    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("Widget settings") },
                navigationIcon = {
                    IconButton(onClick = onCancel) {
                        Icon(Icons.Filled.ArrowBack, contentDescription = "Back")
                    }
                },
            )
        },
        bottomBar = {
            // Sticky actions: Save must never scroll out of reach.
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    // Scaffold does not inset a custom bottom bar for the
                    // system nav bar; without this the actions sit under it
                    // on gesture navigation.
                    .navigationBarsPadding()
                    .padding(horizontal = 16.dp, vertical = 12.dp),
                horizontalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                TextButton(onClick = onCancel, modifier = Modifier.weight(1f)) {
                    Text("Cancel")
                }
                Button(
                    onClick = {
                        onSave(pickedView, pickedDensity, pickedDue, pickedScroll)
                    },
                    modifier = Modifier.weight(2f),
                ) {
                    Text("Save")
                }
            }
        },
    ) { padding ->
        Column(
            modifier = Modifier
                .fillMaxSize()
                .padding(padding)
                .verticalScroll(rememberScrollState())
                .padding(horizontal = 16.dp),
            verticalArrangement = Arrangement.spacedBy(16.dp),
        ) {
            WidgetPreview(
                view = pickedView,
                density = pickedDensity,
                showDue = pickedDue,
                scrollable = pickedScroll,
            )
            SettingsGroup("Tasks shown") {
                SingleChoiceSegmentedButtonRow(Modifier.fillMaxWidth()) {
                    TaskWidgetView.entries.forEachIndexed { i, v ->
                        SegmentedButton(
                            selected = pickedView == v,
                            onClick = { pickedView = v },
                            shape = SegmentedButtonDefaults.itemShape(
                                index = i, count = TaskWidgetView.entries.size),
                        ) { Text(v.title) }
                    }
                }
            }
            SettingsGroup("Row style") {
                SingleChoiceSegmentedButtonRow(Modifier.fillMaxWidth()) {
                    TaskWidgetDensity.entries.forEachIndexed { i, d ->
                        SegmentedButton(
                            selected = pickedDensity == d,
                            onClick = { pickedDensity = d },
                            shape = SegmentedButtonDefaults.itemShape(
                                index = i, count = TaskWidgetDensity.entries.size),
                        ) { Text(d.title) }
                    }
                }
            }
            SettingsGroup("Options") {
                SwitchRow(
                    title = "Show due dates",
                    subtitle = "A friendly line under each task name",
                    checked = pickedDue,
                    onChange = { pickedDue = it },
                )
                SwitchRow(
                    title = "Scrolling list",
                    subtitle = if (pickedScroll) {
                        "All tasks, scroll through them"
                    } else {
                        "First ${TaskWidget.STATIC_ROW_LIMIT} tasks, no scrolling"
                    },
                    checked = pickedScroll,
                    onChange = { pickedScroll = it },
                )
            }
            Spacer(modifier = Modifier.height(4.dp))
        }
    }
}

/** A card per group of controls, with a small heading. */
@Composable
private fun SettingsGroup(title: String, content: @Composable () -> Unit) {
    Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Text(
            title,
            style = MaterialTheme.typography.labelLarge,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        Card(modifier = Modifier.fillMaxWidth()) {
            Column(
                modifier = Modifier.padding(vertical = 8.dp),
                verticalArrangement = Arrangement.spacedBy(4.dp),
            ) { content() }
        }
    }
}

/** One switch with an explanatory subtitle; the whole row toggles. */
@Composable
private fun SwitchRow(
    title: String,
    subtitle: String,
    checked: Boolean,
    onChange: (Boolean) -> Unit,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .selectable(
                selected = checked,
                onClick = { onChange(!checked) },
                role = Role.Switch,
            )
            .padding(horizontal = 16.dp, vertical = 10.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Column(modifier = Modifier.weight(1f)) {
            Text(title, style = MaterialTheme.typography.bodyLarge)
            Text(
                subtitle,
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        Spacer(modifier = Modifier.width(12.dp))
        Switch(checked = checked, onCheckedChange = null)
    }
}

/** Miniature of the real widget, so each choice is visible immediately:
 * same header, row shape, due line and truncation as the placement. */
@Composable
private fun WidgetPreview(
    view: TaskWidgetView,
    density: TaskWidgetDensity,
    showDue: Boolean,
    scrollable: Boolean,
) {
    val compact = density == TaskWidgetDensity.Compact
    val samples = listOf(
        "Apply hair oil" to "Today, 09:00",
        "Bath" to "Tomorrow",
        "Buy vegetables" to "Fri, 18:00",
    )
    // One fake total drives both the count line and the truncation hint, so
    // the preview can't claim one number and demonstrate another.
    val total = if (view == TaskWidgetView.Done) 1 else 12
    val cap = TaskWidget.STATIC_ROW_LIMIT
    val shown = if (scrollable) samples else samples.take(min(samples.size, cap))
    val hidden = (total - shown.size).coerceAtLeast(0)
    Card(
        modifier = Modifier.fillMaxWidth(),
        shape = RoundedCornerShape(24.dp),
        colors = CardDefaults.cardColors(
            containerColor = MaterialTheme.colorScheme.surfaceContainerHighest),
    ) {
        Column(modifier = Modifier.padding(16.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Column(modifier = Modifier.weight(1f)) {
                    Text(
                        "Tasks",
                        style = MaterialTheme.typography.titleSmall,
                        fontWeight = FontWeight.Bold,
                    )
                    Text(
                        previewCount(view, scrollable),
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                Text(
                    view.title,
                    style = MaterialTheme.typography.labelLarge,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                Spacer(modifier = Modifier.width(8.dp))
                Icon(
                    Icons.Filled.Refresh,
                    contentDescription = null,
                    tint = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.size(20.dp),
                )
            }
            HorizontalDivider(
                modifier = Modifier.padding(vertical = 10.dp),
                color = MaterialTheme.colorScheme.outlineVariant,
            )
            shown.forEach { (name, due) ->
                Row(
                    modifier = Modifier
                        .fillMaxWidth()
                        .padding(vertical = if (compact) 2.dp else 4.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Icon(
                        Icons.Filled.RadioButtonUnchecked,
                        contentDescription = null,
                        tint = MaterialTheme.colorScheme.onSurfaceVariant,
                        modifier = Modifier.size(if (compact) 18.dp else 22.dp),
                    )
                    Spacer(modifier = Modifier.width(10.dp))
                    Column(modifier = Modifier.weight(1f)) {
                        Text(
                            name,
                            style = if (compact) {
                                MaterialTheme.typography.bodyMedium
                            } else {
                                MaterialTheme.typography.bodyLarge
                            },
                            maxLines = 1,
                        )
                        if (showDue) {
                            Text(
                                due,
                                style = MaterialTheme.typography.bodySmall,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                                maxLines = 1,
                            )
                        }
                    }
                }
            }
            if (!scrollable && hidden > 0) {
                Text(
                    "+ $hidden more in the app",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(top = 4.dp),
                )
            }
        }
    }
}

/** Honest count line: the non-scrolling mode caps rows, so say so. Uses
 * the same fake total as the preview body. */
private fun previewCount(view: TaskWidgetView, scrollable: Boolean): String {
    val noun = when (view) {
        TaskWidgetView.Open -> "open tasks"
        TaskWidgetView.Done -> "done tasks"
        TaskWidgetView.All -> "tasks"
    }
    val total = if (view == TaskWidgetView.Done) 1 else 12
    return if (scrollable) {
        "$total $noun"
    } else {
        "$total $noun · showing ${TaskWidget.STATIC_ROW_LIMIT}"
    }
}
