plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("org.jetbrains.kotlin.plugin.compose")
    id("org.jetbrains.kotlin.plugin.serialization")
}

android {
    namespace = "com.vishnu.agento"
    compileSdk = 36

    defaultConfig {
        applicationId = "com.vishnu.agento"
        minSdk = 28
        targetSdk = 35
        // CI sets VERSION_CODE to the GitHub run number so every main-branch
        // build sorts higher than the last — required for the in-app updater
        // (Android refuses to install an "update" with a lower/equal code).
        // VERSION_NAME defaults to the tracked release line.
        versionCode = (System.getenv("VERSION_CODE")?.toIntOrNull() ?: 1)
        versionName = (System.getenv("VERSION_NAME").takeUnless { it.isNullOrEmpty() } ?: "0.1.0")
    }

    signingConfigs {
        create("release") {
            // Stable signature is REQUIRED for auto-updates: Android rejects
            // an update signed with a different key. Provide a committed
            // keystore via repo secrets (see android-apk.yml); the generated
            // debug keystore fallback is for PR/ephemeral builds only.
            val keystorePath = System.getenv("KEYSTORE_PATH")
                ?: (System.getProperty("user.home") + "/.android/debug.keystore")
            storeFile = file(keystorePath)
            // takeUnless: unset GitHub secrets arrive as empty strings, not null.
            storePassword = System.getenv("KEYSTORE_PASSWORD").takeUnless { it.isNullOrEmpty() } ?: "android"
            keyAlias = System.getenv("KEY_ALIAS").takeUnless { it.isNullOrEmpty() } ?: "androiddebugkey"
            keyPassword = System.getenv("KEY_PASSWORD").takeUnless { it.isNullOrEmpty() } ?: "android"
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(
                getDefaultProguardFile("proguard-android-optimize.txt"),
                "proguard-rules.pro",
            )
            signingConfig = signingConfigs.getByName("release")
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions {
        jvmTarget = "17"
    }
    buildFeatures {
        compose = true
    }
}

dependencies {
    val composeBom = platform("androidx.compose:compose-bom:2024.12.01")
    implementation(composeBom)

    implementation("androidx.activity:activity-compose:1.9.3")
    implementation("androidx.compose.material3:material3")
    implementation("androidx.compose.ui:ui")
    implementation("androidx.compose.ui:ui-tooling-preview")
    debugImplementation("androidx.compose.ui:ui-tooling")
    implementation("androidx.lifecycle:lifecycle-runtime-compose:2.8.7")
    implementation("androidx.lifecycle:lifecycle-viewmodel-compose:2.8.7")

    implementation("androidx.health.connect:connect-client:1.1.0")

    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.9.0")
    implementation("org.jetbrains.kotlinx:kotlinx-serialization-json:1.7.3")
    implementation("com.squareup.okhttp3:okhttp:4.12.0")

    implementation("androidx.core:core-ktx:1.15.0")
    implementation("androidx.work:work-runtime-ktx:2.10.0")
}
