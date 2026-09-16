package com.vishnu.healthgateway

import android.content.Context
import android.util.Log
import androidx.activity.result.ActivityResultLauncher
import androidx.health.connect.client.HealthConnectClient
import androidx.health.connect.client.permission.HealthPermission
import androidx.health.connect.client.records.ActiveCaloriesBurnedRecord
import androidx.health.connect.client.records.DistanceRecord
import androidx.health.connect.client.records.ExerciseSessionRecord
import androidx.health.connect.client.records.SleepSessionRecord
import androidx.health.connect.client.records.StepsRecord
import androidx.health.connect.client.records.TotalCaloriesBurnedRecord
import androidx.health.connect.client.request.AggregateRequest
import androidx.health.connect.client.request.ReadRecordsRequest
import androidx.health.connect.client.time.TimeRangeFilter
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import java.time.Duration
import java.time.Instant
import java.time.LocalDate
import java.time.ZoneOffset
import java.time.format.DateTimeFormatter

/**
 * Reads health data from Health Connect and shapes it into [HealthSyncPayload]
 * for upload. All reads run on Dispatchers.IO; every method degrades to an
 * empty payload (never throws) when the client is unavailable.
 */
class HealthConnectManager(context: Context) {

    /** Null when Health Connect is missing/unusable — callers treat null as "no data". */
    private val client: HealthConnectClient? = runCatching {
        HealthConnectClient.getOrCreate(context)
    }.getOrNull()

    /** Static helpers: permission set, install diagnostics, timestamp format. */
    companion object {
        private const val TAG = "HealthConnectManager"

        const val HEALTH_CONNECT_PACKAGE = "com.google.android.apps.healthdata"

        /** Every Health Connect permission the app needs (must mirror the manifest). */
        val PERMISSIONS = setOf(
            HealthPermission.getReadPermission(StepsRecord::class),
            HealthPermission.getReadPermission(ActiveCaloriesBurnedRecord::class),
            HealthPermission.getReadPermission(TotalCaloriesBurnedRecord::class),
            HealthPermission.getReadPermission(DistanceRecord::class),
            HealthPermission.getReadPermission(SleepSessionRecord::class),
            HealthPermission.getReadPermission(ExerciseSessionRecord::class),
        )

        private val UTC: DateTimeFormatter = DateTimeFormatter.ISO_INSTANT

        /** Maps the Health Connect SDK status to the app's availability enum. */
        fun availabilityStatus(context: Context): HealthConnectAvailability =
            when (HealthConnectClient.getSdkStatus(context)) {
                HealthConnectClient.SDK_UNAVAILABLE -> HealthConnectAvailability.UNAVAILABLE
                HealthConnectClient.SDK_UNAVAILABLE_PROVIDER_UPDATE_REQUIRED ->
                    HealthConnectAvailability.UPDATE_REQUIRED
                else -> HealthConnectAvailability.AVAILABLE
            }

        /** Human-readable Health Connect install state for the diagnostics UI. */
        fun healthConnectPackageInfo(context: Context): String {
            return try {
                val pm = context.packageManager
                val info = pm.getPackageInfo(HEALTH_CONNECT_PACKAGE, 0)
                "installed v${info.versionName}, enabled=${info.applicationInfo?.enabled}"
            } catch (e: Exception) {
                "NOT installed"
            }
        }

        fun playStoreUrl(): String =
            "https://play.google.com/store/apps/details?id=$HEALTH_CONNECT_PACKAGE"
    }

    /** Currently granted permissions (subset of [PERMISSIONS]). */
    suspend fun grantedPermissions(): Set<String> {
        val c = client ?: return emptySet()
        return withContext(Dispatchers.IO) {
            c.permissionController.getGrantedPermissions()
        }
    }

    /** Launches the system permission sheet; false only if the launch itself fails. */
    fun requestPermissions(
        launcher: ActivityResultLauncher<Set<String>>,
        permissions: Set<String> = PERMISSIONS,
    ): Boolean {
        return runCatching { launcher.launch(permissions) }
            .onFailure { Log.e(TAG, "permission launch failed", it) }
            .isSuccess
    }

    /** Opens the Health Connect manage-data screen (fallback when rationale isn't enough). */
    fun openHealthConnectSettings(context: Context) {
        runCatching {
            context.startActivity(HealthConnectClient.getHealthConnectManageDataIntent(context))
        }.onFailure { Log.e(TAG, "open HC settings failed", it) }
    }

    /**
     * Today's payload: steps + active calories since UTC midnight, sleep and
     * workouts from the last 24h (covers last night's sleep spilling past midnight).
     */
    suspend fun collectToday(): HealthSyncPayload {
        val c = client ?: return HealthSyncPayload(
            syncedAtIso = UTC.format(Instant.now()),
            steps = null,
            activeCaloriesKcal = null,
            sleep = emptyList(),
            workouts = emptyList(),
        )
        return withContext(Dispatchers.IO) {
            val now = Instant.now()
            val startOfDay = LocalDate.now(ZoneOffset.UTC)
                .atStartOfDay(ZoneOffset.UTC)
                .toInstant()

            val steps = aggregateSteps(c, startOfDay, now)
            val calories = aggregateActiveCalories(c, startOfDay, now)
            val sleep = readSleep(c, startOfDay.minusSeconds(86400), now)
            val workouts = readWorkouts(c, startOfDay.minusSeconds(86400), now)

            HealthSyncPayload(
                syncedAtIso = UTC.format(now),
                steps = steps,
                activeCaloriesKcal = calories,
                sleep = sleep,
                workouts = workouts,
            )
        }
    }

    /**
     * One payload per day for the last [days] days (first install / reinstall).
     * Days with neither steps nor calories are skipped; sleep + workouts ride
     * on the most recent payload since the server dedupes by timestamp.
     */
    suspend fun collectBackfill(days: Int = 30): List<HealthSyncPayload> {
        val c = client ?: return emptyList()
        return withContext(Dispatchers.IO) {
            val now = Instant.now()
            val start = now.minusSeconds(days * 86400L)
            val sleep = readSleep(c, start, now)
            val workouts = readWorkouts(c, start, now)

            val payloads = mutableListOf<HealthSyncPayload>()
            for (i in 0 until days) {
                val dayStart = start.plusSeconds(i * 86400L)
                val dayEnd = minOf(dayStart.plusSeconds(86400L), now)
                if (dayEnd <= dayStart) break
                val steps = aggregateSteps(c, dayStart, dayEnd)
                val calories = aggregateActiveCalories(c, dayStart, dayEnd)
                if (steps == null && calories == null) continue
                payloads.add(
                    HealthSyncPayload(
                        syncedAtIso = UTC.format(dayStart),
                        steps = steps,
                        activeCaloriesKcal = calories,
                        sleep = emptyList(),
                        workouts = emptyList(),
                    )
                )
            }
            payloads.lastOrNull()?.let {
                payloads[payloads.size - 1] = it.copy(sleep = sleep, workouts = workouts)
            }
            payloads
        }
    }

    /** Total steps in [start, end] via an aggregate query (null = no data). */
    private suspend fun aggregateSteps(client: HealthConnectClient, start: Instant, end: Instant): Long? {
        val response = client.aggregate(
            AggregateRequest(
                metrics = setOf(StepsRecord.COUNT_TOTAL),
                timeRangeFilter = TimeRangeFilter.between(start, end),
            )
        )
        return response[StepsRecord.COUNT_TOTAL]
    }

    /** Active calories in kcal in [start, end] (excludes basal metabolic rate). */
    private suspend fun aggregateActiveCalories(client: HealthConnectClient, start: Instant, end: Instant): Double? {
        val response = client.aggregate(
            AggregateRequest(
                metrics = setOf(ActiveCaloriesBurnedRecord.ACTIVE_CALORIES_TOTAL),
                timeRangeFilter = TimeRangeFilter.between(start, end),
            )
        )
        return response[ActiveCaloriesBurnedRecord.ACTIVE_CALORIES_TOTAL]?.inKilocalories
    }

    /** Sleep sessions in [start, end] with per-stage minute counts. */
    private suspend fun readSleep(client: HealthConnectClient, start: Instant, end: Instant): List<SleepEntry> {
        val records = client.readRecords(
            ReadRecordsRequest(
                recordType = SleepSessionRecord::class,
                timeRangeFilter = TimeRangeFilter.between(start, end),
            )
        )
        return records.records.map { record ->
            val stageCounts = record.stages.groupingBy { it.stage }
                .eachCount()
                .mapKeys { (stageInt, _) ->
                    SleepSessionRecord.STAGE_TYPE_INT_TO_STRING_MAP[stageInt] ?: "UNKNOWN"
                }
                .mapValues { (_, count) -> count.toLong() }
            SleepEntry(
                startIso = record.startTime.toString(),
                endIso = record.endTime.toString(),
                totalMinutes = Duration
                    .between(record.startTime, record.endTime).toMinutes(),
                stages = stageCounts,
            )
        }
    }

    /** Exercise sessions enriched with distance + calories joined by record id. */
    private suspend fun readWorkouts(client: HealthConnectClient, start: Instant, end: Instant): List<WorkoutEntry> {
        val sessions = client.readRecords(
            ReadRecordsRequest(
                recordType = ExerciseSessionRecord::class,
                timeRangeFilter = TimeRangeFilter.between(start, end),
            )
        ).records

        val distanceBySession = readDistanceAggregates(client, start, end)
        val caloriesBySession = readCaloriesAggregates(client, start, end)

        return sessions.map { session ->
            WorkoutEntry(
                startIso = session.startTime.toString(),
                endIso = session.endTime.toString(),
                title = session.title ?: "Workout",
                type = session.exerciseType.toString(),
                distanceMeters = distanceBySession[session.metadata.id],
                caloriesKcal = caloriesBySession[session.metadata.id],
            )
        }
    }

    /** Distance records keyed by session id for the workout join. */
    private suspend fun readDistanceAggregates(client: HealthConnectClient, start: Instant, end: Instant): Map<String, Double> {
        val records = client.readRecords(
            ReadRecordsRequest(
                recordType = DistanceRecord::class,
                timeRangeFilter = TimeRangeFilter.between(start, end),
            )
        ).records
        return records.associate { it.metadata.id to it.distance.inMeters }
    }

    /** Total-calories records keyed by session id for the workout join. */
    private suspend fun readCaloriesAggregates(client: HealthConnectClient, start: Instant, end: Instant): Map<String, Double> {
        val records = client.readRecords(
            ReadRecordsRequest(
                recordType = TotalCaloriesBurnedRecord::class,
                timeRangeFilter = TimeRangeFilter.between(start, end),
            )
        ).records
        return records.associate { it.metadata.id to it.energy.inKilocalories }
    }
}

/** Install/readiness state of Health Connect on this device. */
enum class HealthConnectAvailability {
    AVAILABLE,
    UPDATE_REQUIRED,
    UNAVAILABLE,
}
