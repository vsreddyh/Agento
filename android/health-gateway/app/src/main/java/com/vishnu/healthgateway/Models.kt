package com.vishnu.healthgateway

import kotlinx.serialization.Serializable

/** Wire payload posted to health-api; nulls mean Health Connect had no data. */
@Serializable
data class HealthSyncPayload(
    val device: String = "Redmi Watch 5 Lite",
    val syncedAtIso: String,
    val steps: Long? = null,
    val activeCaloriesKcal: Double? = null,
    val sleep: List<SleepEntry> = emptyList(),
    val workouts: List<WorkoutEntry> = emptyList(),
)

/** One sleep session; stages map Health Connect stage names to minutes. */
@Serializable
data class SleepEntry(
    val startIso: String,
    val endIso: String,
    val totalMinutes: Long,
    val stages: Map<String, Long> = emptyMap(),
)

/** One workout session; type follows Health Connect exercise types. */
@Serializable
data class WorkoutEntry(
    val startIso: String,
    val endIso: String,
    val title: String,
    val type: String,
    val distanceMeters: Double? = null,
    val caloriesKcal: Double? = null,
)

/** Sync outcome surfaced in Settings; message is truncated before display. */
@Serializable
data class SyncResult(
    val success: Boolean,
    val statusCode: Int? = null,
    val message: String = "",
)
