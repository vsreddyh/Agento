package com.vishnu.agento

import android.appwidget.AppWidgetManager
import android.content.Context
import android.content.Intent
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.ArrowBack
import androidx.compose.material.icons.filled.RadioButtonUnchecked
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SegmentedButton
import androidx.compose.material3.SegmentedButtonDefaults
import androidx.compose.material3.SingleChoiceSegmentedButtonRow
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.clearAndSetSemantics
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
    // rememberSaveable: rotating before Save must not reset the picks
    // (this screen is a system-launched configure activity, so rotation
    // is routine).
    var pickedView by rememberSaveable { mutableStateOf(view) }
    var pickedDensity by rememberSaveable { mutableStateOf(density) }
    var pickedDue by rememberSaveable { mutableStateOf(showDue) }
    var pickedScroll by rememberSaveable { mutableStateOf(scrollable) }

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
                // Horizontal padding too: the segmented rows were touching
                // the card edges.
                modifier = Modifier.padding(horizontal = 12.dp, vertical = 12.dp),
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
        // The row is the single switch control; the visual switch is
        // decoration, so hide it from TalkBack to avoid two switch nodes.
        Switch(
            checked = checked,
            onCheckedChange = null,
            modifier = Modifier.clearAndSetSemantics { },
        )
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
    // A full set of samples, so the non-scrolling mode can actually
    // demonstrate its cap instead of implying it with 3 rows and "showing 8".
    val samples = listOf(
        "Apply hair oil" to "Today, 09:00",
        "Bath" to "Today, 20:00",
        "Brush teeth" to "Today, 22:00",
        "Buy vegetables" to "Tomorrow, 18:00",
        "Change bedsheets" to "Fri, 11:00",
        "Water plants" to "Sat, 09:00",
        "Pay rent" to "Sun, 10:00",
        "Plan the week" to "Mon, 08:00",
    )
    // One fake total drives the count line, the rows shown and the
    // truncation hint, so the preview can't claim one number and
    // demonstrate another.
    val total = if (view == TaskWidgetView.Done) 1 else 12
    val cap = TaskWidget.STATIC_ROW_LIMIT
    val shown = if (scrollable) {
        // Still capped by the total: the Done view has one task, so the
        // scrollable preview must not draw eight rows for it.
        samples.take(minOf(total, samples.size))
    } else {
        samples.take(minOf(total, cap))
    }
    val hidden = (total - shown.size).coerceAtLeast(0)
    Card(
        modifier = Modifier.fillMaxWidth(),
        shape = RoundedCornerShape(24.dp),
        colors = CardDefaults.cardColors(
            // surfaceVariant, not surfaceContainerHighest: the rest of the
            // app only uses the former, so this can't drift on an M3 bump.
            containerColor = MaterialTheme.colorScheme.surfaceVariant),
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
            if (hidden > 0) {
                Text(
                    if (scrollable) {
                        "Scroll for $hidden more"
                    } else {
                        "+ $hidden more in the app"
                    },
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(top = 4.dp),
                )
            }
        }
    }
}

/** Honest count line: the non-scrolling mode caps rows, so say so — and
 * never claim a cap above the total it is capping. */
private fun previewCount(view: TaskWidgetView, scrollable: Boolean): String {
    val noun = when (view) {
        TaskWidgetView.Open -> "open task"
        TaskWidgetView.Done -> "done task"
        TaskWidgetView.All -> "task"
    }
    val total = if (view == TaskWidgetView.Done) 1 else 12
    val shown = total.toString() +
        if (total == 1) " $noun" else " ${noun}s"
    return if (scrollable) {
        shown
    } else {
        val cap = minOf(total, TaskWidget.STATIC_ROW_LIMIT)
        "$shown · showing $cap"
    }
}
