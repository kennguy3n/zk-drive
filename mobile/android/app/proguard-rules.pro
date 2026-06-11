# --- UniFFI / JNA -----------------------------------------------------------
# The generated bindings reach the native library through JNA reflection and
# direct-mapped callbacks; keep JNA and the generated uniffi package intact so
# R8 cannot strip or rename the symbols the .so resolves at runtime.
-keep class com.sun.jna.** { *; }
-keep class * extends com.sun.jna.** { *; }
-keepclassmembers class * extends com.sun.jna.** { *; }
-dontwarn java.awt.**
-keep class uniffi.** { *; }

# --- AppAuth ------------------------------------------------------------------
-keep class net.openid.appauth.** { *; }

# --- Kotlinx serialization ----------------------------------------------------
# Keep the @Serializable metadata + generated serializers for our DTOs.
-keepattributes *Annotation*, InnerClasses
-dontnote kotlinx.serialization.**
-keepclassmembers class com.zkdrive.app.data.** {
    *** Companion;
}
-keepclasseswithmembers class com.zkdrive.app.data.** {
    kotlinx.serialization.KSerializer serializer(...);
}
