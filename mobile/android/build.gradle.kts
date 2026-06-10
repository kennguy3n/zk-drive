// Top-level build file. Plugin versions are declared here (apply false) and
// resolved from the version catalog; modules opt in by id in their own
// build scripts. Keeps a single source of truth for every plugin version.
plugins {
    alias(libs.plugins.android.application) apply false
    alias(libs.plugins.kotlin.android) apply false
    alias(libs.plugins.kotlin.compose) apply false
    alias(libs.plugins.kotlin.serialization) apply false
    alias(libs.plugins.ksp) apply false
    alias(libs.plugins.hilt) apply false
}
