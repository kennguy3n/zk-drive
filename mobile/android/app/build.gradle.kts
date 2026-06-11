import java.util.Properties

plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.android)
    alias(libs.plugins.kotlin.compose)
    alias(libs.plugins.kotlin.serialization)
    alias(libs.plugins.ksp)
    alias(libs.plugins.hilt)
}

// ---------------------------------------------------------------------------
// Rust FFI bridge integration.
//
// The native app consumes the SAME UniFFI bridge the iOS app uses. Rather
// than committing generated bindings (which can silently drift from the
// shipped .so), we regenerate them from the bridge crate at build time:
// `build-android.sh` cross-compiles libzk_mobile_bridge.so for every ABI
// and emits the Kotlin bindings, then we put both on the app's source sets.
// ---------------------------------------------------------------------------
val rustBridgeDir = rootProject.layout.projectDirectory.dir("../rust-bridge")
val bridgeOutDir = rustBridgeDir.dir("build/android")
val bridgeJniLibs = bridgeOutDir.dir("jniLibs")
val bridgeKotlin = bridgeOutDir.dir("kotlin")

// Resolve the NDK location at configuration time from env vars or
// local.properties (ndk.dir). Computed to a plain String so the task closure
// captures no Gradle script references — required for the configuration cache.
val resolvedNdkDir: String? = run {
    System.getenv("ANDROID_NDK_HOME")?.takeIf { it.isNotBlank() }
        ?: System.getenv("ANDROID_NDK_ROOT")?.takeIf { it.isNotBlank() }
        ?: rootProject.file("local.properties")
            .takeIf { it.exists() }
            ?.let { f -> Properties().apply { f.inputStream().use { load(it) } }.getProperty("ndk.dir") }
            ?.takeIf { it.isNotBlank() }
}

val bridgeOutDirPath = bridgeOutDir.asFile.absolutePath

val buildRustBridge by tasks.registering(Exec::class) {
    group = "build"
    description = "Cross-compiles the Rust mobile bridge for Android and generates Kotlin bindings."
    workingDir = rustBridgeDir.asFile

    // Re-run only when the bridge sources change; the generated bindings +
    // jniLibs are the declared outputs so downstream Kotlin compilation gets
    // a proper task dependency and incremental up-to-date checks.
    inputs.dir(rustBridgeDir.dir("src"))
    inputs.file(rustBridgeDir.file("Cargo.toml"))
    inputs.file(rustBridgeDir.file("build-android.sh"))
    outputs.dir(bridgeKotlin)
    outputs.dir(bridgeJniLibs)
    outputs.cacheIf { true }

    commandLine("bash", "build-android.sh")

    // build-android.sh emits into ./build/android by default. The NDK path is
    // resolved at configuration time (env vars or local.properties); if it is
    // absent, build-android.sh fails fast with a clear cargo-ndk error. No
    // doFirst here — capturing script state would break the configuration cache.
    environment("OUT_DIR", bridgeOutDirPath)
    resolvedNdkDir?.let { environment("ANDROID_NDK_HOME", it) }
}

// ---------------------------------------------------------------------------
// OAuth / server configuration.
//
// These come from a git-ignored `oauth.properties` (developer machines) or
// environment variables (CI), never hard-coded secrets. Defaults are safe,
// non-secret placeholders so a fresh checkout still configures + compiles.
// Firebase is configured the same way (no committed google-services.json),
// so push is enabled only when the FCM_* values are supplied.
// ---------------------------------------------------------------------------
val oauthProps = Properties().apply {
    val f = rootProject.file("oauth.properties")
    if (f.exists()) f.inputStream().use { load(it) }
}

fun cfg(key: String, env: String, default: String): String =
    (oauthProps.getProperty(key) ?: System.getenv(env) ?: default)

android {
    namespace = "com.zkdrive.app"
    compileSdk = 35

    // Pin to NDK r26d — the same toolchain build-android.sh uses to
    // cross-compile the Rust bridge. Matching versions lets AGP strip debug
    // symbols from the shipped .so files (otherwise they ship unstripped).
    ndkVersion = "26.3.11579264"

    defaultConfig {
        applicationId = "com.zkdrive.app"
        minSdk = 31
        targetSdk = 35
        versionCode = 1
        versionName = "1.0.0"

        testInstrumentationRunner = "androidx.test.runner.AndroidJUnitRunner"

        // Only ship the ABIs the bridge builds for. (No 32-bit x86 emulator
        // image and no armeabi beyond v7a.)
        ndk {
            abiFilters += listOf("arm64-v8a", "armeabi-v7a", "x86_64")
        }

        // --- OAuth2 / OIDC (iam-core) -------------------------------------
        buildConfigField("String", "OIDC_ISSUER", "\"${cfg("oidc.issuer", "ZK_OIDC_ISSUER", "https://iam.zkdrive.example.com")}\"")
        buildConfigField("String", "OIDC_CLIENT_ID", "\"${cfg("oidc.clientId", "ZK_OIDC_CLIENT_ID", "zk-drive-android")}\"")
        buildConfigField("String", "OIDC_SCOPE", "\"${cfg("oidc.scope", "ZK_OIDC_SCOPE", "openid profile email offline_access")}\"")
        buildConfigField("String", "API_BASE_URL", "\"${cfg("api.baseUrl", "ZK_API_BASE_URL", "https://api.zkdrive.example.com")}\"")
        // Public web origin that serves the /share/{token} landing page. Falls
        // back to the API base when a deployment co-hosts web + API.
        buildConfigField("String", "WEB_BASE_URL", "\"${cfg("web.baseUrl", "ZK_WEB_BASE_URL", "https://app.zkdrive.example.com")}\"")

        // The OAuth redirect URI host segment used both for the AppAuth
        // redirect and the manifest intent-filter (appAuthRedirectScheme).
        val redirectScheme = cfg("oidc.redirectScheme", "ZK_OIDC_REDIRECT_SCHEME", "com.zkdrive.app")
        buildConfigField("String", "OIDC_REDIRECT_URI", "\"$redirectScheme:/oauth2redirect\"")
        manifestPlaceholders["appAuthRedirectScheme"] = redirectScheme

        // --- Firebase Cloud Messaging (optional; runtime-initialised) -----
        buildConfigField("String", "FCM_PROJECT_ID", "\"${cfg("fcm.projectId", "ZK_FCM_PROJECT_ID", "")}\"")
        buildConfigField("String", "FCM_APP_ID", "\"${cfg("fcm.appId", "ZK_FCM_APP_ID", "")}\"")
        buildConfigField("String", "FCM_API_KEY", "\"${cfg("fcm.apiKey", "ZK_FCM_API_KEY", "")}\"")
        buildConfigField("String", "FCM_SENDER_ID", "\"${cfg("fcm.senderId", "ZK_FCM_SENDER_ID", "")}\"")
    }

    // ----- signing -----------------------------------------------------------
    // Release signing reads an UPLOAD keystore supplied entirely through env
    // vars (CI secrets). No keystore is committed; when the env is absent the
    // release variant simply builds unsigned so local `assembleRelease` works.
    val keystorePath = System.getenv("ZK_ANDROID_KEYSTORE")
    signingConfigs {
        if (!keystorePath.isNullOrBlank() && file(keystorePath).exists()) {
            create("release") {
                storeFile = file(keystorePath)
                storePassword = System.getenv("ZK_ANDROID_KEYSTORE_PASSWORD")
                keyAlias = System.getenv("ZK_ANDROID_KEY_ALIAS")
                keyPassword = System.getenv("ZK_ANDROID_KEY_PASSWORD")
            }
        }
    }

    buildTypes {
        debug {
            applicationIdSuffix = ".debug"
            isDebuggable = true
        }
        release {
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(
                getDefaultProguardFile("proguard-android-optimize.txt"),
                "proguard-rules.pro",
            )
            signingConfig = signingConfigs.findByName("release")
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
        buildConfig = true
    }

    // Bring the generated UniFFI bindings + native libraries into the build.
    sourceSets["main"].java.srcDir(bridgeKotlin)
    sourceSets["main"].jniLibs.srcDir(bridgeJniLibs)

    packaging {
        resources {
            excludes += "/META-INF/{AL2.0,LGPL2.1}"
        }
        // JNA ships its own .so per ABI; let the most specific win.
        jniLibs.pickFirsts += "**/libjnidispatch.so"
    }

    lint {
        warningsAsErrors = false
        abortOnError = true
        disable += setOf("GradleDependency", "NewerVersionAvailable")
        // Scoped suppressions (e.g. NewApi in the generated UniFFI bindings)
        // live in lint.xml so they stay narrow and auditable.
        lintConfig = file("lint.xml")
    }
}

// Kotlin compilation (and therefore the whole build) depends on the
// generated bindings being present.
tasks.withType<org.jetbrains.kotlin.gradle.tasks.KotlinCompile>().configureEach {
    dependsOn(buildRustBridge)
}
// The merge*JniLibFolders tasks consume the bridge .so outputs.
tasks.matching { it.name.startsWith("merge") && it.name.contains("JniLibFolders") }
    .configureEach { dependsOn(buildRustBridge) }

dependencies {
    implementation(libs.androidx.core.ktx)
    implementation(libs.androidx.splashscreen)
    implementation(libs.androidx.lifecycle.runtime.ktx)
    implementation(libs.androidx.lifecycle.runtime.compose)
    implementation(libs.androidx.lifecycle.viewmodel.compose)
    implementation(libs.androidx.activity.compose)

    implementation(platform(libs.androidx.compose.bom))
    implementation(libs.androidx.compose.ui)
    implementation(libs.androidx.compose.ui.graphics)
    implementation(libs.androidx.compose.ui.tooling.preview)
    implementation(libs.androidx.compose.material3)
    implementation(libs.androidx.compose.material.icons.extended)
    implementation(libs.androidx.navigation.compose)
    debugImplementation(libs.androidx.compose.ui.tooling)

    implementation(libs.androidx.coroutines.android)
    implementation(libs.coroutines.play.services)
    implementation(libs.kotlinx.serialization.json)

    implementation(libs.hilt.android)
    ksp(libs.hilt.compiler)
    implementation(libs.hilt.navigation.compose)
    implementation(libs.hilt.work)
    ksp(libs.hilt.work.compiler)

    implementation(libs.retrofit)
    implementation(libs.retrofit.serialization)
    implementation(libs.okhttp)
    implementation(libs.okhttp.logging)

    implementation(libs.androidx.security.crypto)
    implementation(libs.androidx.biometric)
    implementation(libs.appauth)
    implementation(libs.androidx.browser)
    implementation(libs.androidx.datastore.preferences)
    implementation(libs.androidx.work.runtime)

    implementation(libs.coil.compose)

    // UniFFI Kotlin bindings call into the .so through JNA.
    implementation(libs.jna) { artifact { type = "aar" } }

    implementation(platform(libs.firebase.bom))
    implementation(libs.firebase.messaging)

    testImplementation(libs.junit)
    testImplementation(libs.kotlinx.coroutines.test)
    testImplementation(libs.mockk)
    testImplementation(libs.turbine)
    androidTestImplementation(libs.androidx.test.junit)
    androidTestImplementation(libs.androidx.espresso.core)
    androidTestImplementation(platform(libs.androidx.compose.bom))
    androidTestImplementation(libs.androidx.compose.ui.test.junit4)
    debugImplementation(libs.androidx.compose.ui.test.manifest)
}
