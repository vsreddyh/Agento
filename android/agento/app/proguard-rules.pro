# kotlinx.serialization — keep serializer classes for @Serializable models
-keepattributes *Annotation*, InnerClasses
-dontnote kotlinx.serialization.AnnotationsKt
-keepclassmembers class com.vishnu.agento.** {
    *** Companion;
}
-keepclasseswithmembers class com.vishnu.agento.** {
    kotlinx.serialization.KSerializer serializer(...);
}
-keep,includedescriptorclasses class com.vishnu.agento.**$$serializer { *; }
-keepclassmembers class com.vishnu.agento.** {
    *** INSTANCE;
    kotlinx.serialization.KSerializer serializer(...);
}
-keep,includedescriptorclasses class com.vishnu.agento.HealthSyncPayload,
    com.vishnu.agento.SleepEntry,
    com.vishnu.agento.WorkoutEntry,
    com.vishnu.agento.SyncResult { *; }

# Health Connect client uses reflection on records
-keep class androidx.health.connect.client.records.** { *; }
-dontwarn androidx.health.connect.client.**

# WorkManager — entry points loaded via Class.forName
-keep class * extends androidx.work.Worker { <init>(android.content.Context, androidx.work.WorkerParameters); }
-keep class * extends androidx.work.ListenableWorker { <init>(android.content.Context, androidx.work.WorkerParameters); }
-dontwarn androidx.work.**

# OkHttp
-dontwarn okhttp3.**
-dontwarn okio.**
-dontwarn org.conscrypt.**
-dontwarn org.bouncycastle.**
-dontwarn org.openjsse.**

# Coroutines
-dontwarn kotlinx.coroutines.**
