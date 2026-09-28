package com.vishnu.agento

import android.appwidget.AppWidgetManager
import android.content.Context
import android.content.Intent
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.Checkbox
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.unit.dp

/**
 * Display settings for one task-widget placement: which slice it shows,
 * row density, the due line, and the diagnostic style ladder (#137).
 * Launched by the system at add time (android:configure) and from the
 * Task Manager screen afterwards. Cancelling at add time aborts the
 * placement; saving re-renders that widget via [TaskWidget].
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
                Surface(
                    modifier = Modifier.fillMaxWidth(),
                    color = MaterialTheme.colorScheme.background,
                ) {
                    var density by remember {
                        mutableStateOf(TaskWidget.densityFor(this, appWidgetId))
                    }
                    var showDue by remember {
                        mutableStateOf(TaskWidget.showDueFor(this, appWidgetId))
                    }
                    // Removed: the standalone "Scrolling list" checkbox —
                    // the style ladder's D/E own that choice (#137).
                    var style by remember {
                        mutableStateOf(TaskWidget.styleFor(this, appWidgetId))
                    }
                    // The widget's own header toggle never rendered on the
                    // affected launcher (#137), so view switching moved here
                    // rather than being dropped.
                    var widgetView by remember {
                        mutableStateOf(TaskWidget.viewFor(this, appWidgetId))
                    }
                    // Scrollable: view + density + due + the style ladder
                    // push Save off-screen on small screens / large fonts.
                    Column(
                        modifier = Modifier
                            .verticalScroll(rememberScrollState())
                            .padding(20.dp),
                    ) {
                        Text(
                            getString(R.string.task_widget_display),
                            style = MaterialTheme.typography.headlineSmall,
                        )
                        Spacer(modifier = Modifier.height(16.dp))
                        Text(
                            "Show",
                            style = MaterialTheme.typography.labelLarge,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                        TaskWidgetView.entries.forEach { v ->
                            Row(
                                modifier = Modifier.fillMaxWidth()
                                    .selectable(
                                        selected = widgetView == v,
                                        onClick = { widgetView = v },
                                        role = Role.RadioButton,
                                    )
                                    .padding(vertical = 6.dp),
                                verticalAlignment = Alignment.CenterVertically,
                            ) {
                                RadioButton(selected = widgetView == v, onClick = null)
                                Text(
                                    v.title,
                                    modifier = Modifier.padding(start = 8.dp),
                                )
                            }
                        }
                        Text(
                            "Density",
                            style = MaterialTheme.typography.labelLarge,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                        TaskWidgetDensity.entries.forEach { d ->
                            Row(
                                modifier = Modifier.fillMaxWidth()
                                    .selectable(
                                        selected = density == d,
                                        onClick = { density = d },
                                        role = Role.RadioButton,
                                    )
                                    .padding(vertical = 8.dp),
                                verticalAlignment = Alignment.CenterVertically,
                            ) {
                                RadioButton(
                                    selected = density == d,
                                    onClick = null,
                                )
                                Text(
                                    d.title,
                                    modifier = Modifier.padding(start = 8.dp),
                                )
                            }
                        }
                        Row(
                            modifier = Modifier.fillMaxWidth()
                                .selectable(
                                    selected = showDue,
                                    onClick = { showDue = !showDue },
                                    role = Role.Checkbox,
                                )
                                .padding(vertical = 8.dp),
                            verticalAlignment = Alignment.CenterVertically,
                        ) {
                            Checkbox(checked = showDue, onCheckedChange = null)
                            Text(
                                "Show due dates",
                                modifier = Modifier.padding(start = 8.dp),
                            )
                        }
                        // Diagnostic style ladder (#137): C, then D1→D5 each
                        // add one more element, then E (the collection
                        // widget). C is the header known to render.
                        // back at a time so the failing piece is identified
                        // in one install. E is the real widget; the ladder
                        // also owns scrolling vs plain rows (D/E), so there
                        // is no separate checkbox to disagree with it.
                        Text(
                            getString(R.string.task_widget_style_label),
                            style = MaterialTheme.typography.labelLarge,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                            modifier = Modifier.padding(top = 8.dp),
                        )
                        listOf(
                            TaskWidget.STYLE_CHROME to R.string.task_widget_style_c,
                            TaskWidget.STYLE_ROWS to R.string.task_widget_style_d1,
                            TaskWidget.STYLE_ROWS_DIVIDER to R.string.task_widget_style_d2,
                            TaskWidget.STYLE_ROWS_EMPTY to R.string.task_widget_style_d3,
                            TaskWidget.STYLE_PLUS_TOGGLE to R.string.task_widget_style_d4,
                            TaskWidget.STYLE_FULL_STATIC to R.string.task_widget_style_d5,
                            TaskWidget.STYLE_FULL_SCROLL to R.string.task_widget_style_e,
                        ).forEach { (value, labelRes) ->
                            Row(
                                modifier = Modifier.fillMaxWidth()
                                    .selectable(
                                        selected = style == value,
                                        onClick = { style = value },
                                        role = Role.RadioButton,
                                    )
                                    .padding(vertical = 6.dp),
                                verticalAlignment = Alignment.CenterVertically,
                            ) {
                                RadioButton(selected = style == value, onClick = null)
                                Text(
                                    getString(labelRes),
                                    modifier = Modifier.padding(start = 8.dp),
                                )
                            }
                        }
                        Spacer(modifier = Modifier.height(16.dp))
                        Row(
                            modifier = Modifier.fillMaxWidth(),
                            horizontalArrangement = Arrangement.End,
                        ) {
                            TextButton(onClick = {
                                setResult(RESULT_CANCELED, done)
                                finish()
                            }) { Text("Cancel") }
                            Button(onClick = {
                                prefs().edit()
                                    .putString(
                                        "task_widget_density_$appWidgetId",
                                        density.name,
                                    )
                                    .putBoolean("task_widget_due_$appWidgetId", showDue)
                                    // Style owns scrolling: keep the old
                                    // pref in sync so it still reads
                                    // correctly after the ladder is removed.
                                    .putBoolean(
                                        "task_widget_scroll_$appWidgetId",
                                        style == TaskWidget.STYLE_FULL_SCROLL)
                                    .putInt("task_widget_style_$appWidgetId", style)
                                    .putString(
                                        "task_widget_view_$appWidgetId",
                                        widgetView.name,
                                    )
                                    .apply()
                                TaskWidget.refresh(this@TaskWidgetConfigActivity)
                                setResult(RESULT_OK, done)
                                finish()
                            }) { Text("Save") }
                        }
                    }
                }
            }
        }
    }

    private fun prefs() = applicationContext.getSharedPreferences(
        AgentoApp.PREFS_NAME, Context.MODE_PRIVATE)
}
