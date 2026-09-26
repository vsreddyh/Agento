package com.vishnu.agento

import android.app.PendingIntent
import android.appwidget.AppWidgetManager
import android.appwidget.AppWidgetProvider
import android.content.Context
import android.content.Intent
import android.widget.RemoteViews

/**
 * One-button home-screen widget (#85): tap to open God straight into a
 * live voice session. No configuration, no preview of state — the button
 * just fires MainActivity with the LIVE action.
 */
class LiveWidget : AppWidgetProvider() {

    override fun onUpdate(
        context: Context,
        appWidgetManager: AppWidgetManager,
        appWidgetIds: IntArray,
    ) {
        for (id in appWidgetIds) {
            val intent = Intent(context, MainActivity::class.java)
                .setAction(ACTION_LIVE)
                .putExtra(EXTRA_TAB, "god")
            val pending = PendingIntent.getActivity(
                context, id, intent,
                PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
            )
            appWidgetManager.updateAppWidget(
                id,
                RemoteViews(context.packageName, R.layout.live_widget).apply {
                    setOnClickPendingIntent(R.id.live_button, pending)
                },
            )
        }
    }

    companion object {
        const val ACTION_LIVE = "com.vishnu.agento.action.LIVE"
        const val EXTRA_TAB = "com.vishnu.agento.extra.TAB"
    }
}
