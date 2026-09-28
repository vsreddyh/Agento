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
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.selection.selectable
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
 * Display settings for one task-widget placement: row density and
 * whether rows show the due line. Launched by the system at add time
 * (android:configure) and later by tapping the widget's title, which
 * passes the placement id for reconfiguration. Cancelling at add time
 * aborts the placement; saving re-renders that widget via [TaskWidget].
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
                    var scrollable by remember {
                        mutableStateOf(TaskWidget.scrollableFor(this, appWidgetId))
                    }
                    Column(modifier = Modifier.padding(20.dp)) {
                        Text(
                            getString(R.string.task_widget_display),
                            style = MaterialTheme.typography.headlineSmall,
                        )
                        Spacer(modifier = Modifier.height(16.dp))
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
                        Row(
                            modifier = Modifier.fillMaxWidth()
                                .selectable(
                                    selected = !scrollable,
                                    onClick = { scrollable = !scrollable },
                                    role = Role.Checkbox,
                                )
                                .padding(vertical = 8.dp),
                            verticalAlignment = Alignment.CenterVertically,
                        ) {
                            Checkbox(checked = !scrollable, onCheckedChange = null)
                            Column(modifier = Modifier.padding(start = 8.dp)) {
                                Text("Static rows")
                                Text(
                                    "No scrolling list — use when the widget shows a load error",
                                    style = MaterialTheme.typography.bodySmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
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
                                    .putBoolean("task_widget_scroll_$appWidgetId", scrollable)
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
