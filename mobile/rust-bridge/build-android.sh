#!/usr/bin/env bash
#
# build-android.sh — cross-compile the ZK Drive mobile bridge for Android
# and emit jniLibs + generated Kotlin bindings.
#
# Output layout (under $OUT_DIR, default ./build/android):
#   jniLibs/arm64-v8a/libzk_mobile_bridge.so
#   jniLibs/armeabi-v7a/libzk_mobile_bridge.so
#   jniLibs/x86_64/libzk_mobile_bridge.so
#   kotlin/uniffi/zk_mobile_bridge/zk_mobile_bridge.kt
#
# The Android app module consumes this by putting `jniLibs/` on its
# `sourceSets[...].jniLibs.srcDirs` and the generated `.kt` on its
# source path (or by wrapping both in an AAR). The single Kotlin file is
# generated from the SAME crate metadata as the Swift bindings produced
# by build-ios.sh, so the two platforms share one contract.
#
# Requirements:
#   * Rust toolchain with the Android targets installed:
#       rustup target add aarch64-linux-android armv7-linux-androideabi x86_64-linux-android
#   * cargo-ndk:  cargo install cargo-ndk
#   * Android NDK r26+, located via $ANDROID_NDK_HOME or $ANDROID_NDK_ROOT.
#
# Env knobs:
#   ANDROID_NDK_HOME / ANDROID_NDK_ROOT   path to the NDK (required)
#   OUT_DIR                                output root (default ./build/android)
#   PROFILE                                release|debug (default release)
#   ANDROID_API                            min API level (default 24)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

OUT_DIR="${OUT_DIR:-$SCRIPT_DIR/build/android}"
PROFILE="${PROFILE:-release}"
ANDROID_API="${ANDROID_API:-24}"
LIB_NAME="libzk_mobile_bridge.so"

# cargo-ndk ABI name -> rust target triple (used to locate the lib we
# feed to uniffi-bindgen's library mode).
ABIS=(arm64-v8a armeabi-v7a x86_64)
declare -A ABI_TRIPLE=(
  [arm64-v8a]=aarch64-linux-android
  [armeabi-v7a]=armv7-linux-androideabi
  [x86_64]=x86_64-linux-android
)

# --- preflight -------------------------------------------------------------
NDK="${ANDROID_NDK_HOME:-${ANDROID_NDK_ROOT:-}}"
if [[ -z "$NDK" || ! -d "$NDK" ]]; then
  echo "error: set ANDROID_NDK_HOME (or ANDROID_NDK_ROOT) to a valid NDK r26+ install" >&2
  exit 1
fi
export ANDROID_NDK_HOME="$NDK"
if ! command -v cargo-ndk >/dev/null 2>&1; then
  echo "error: cargo-ndk not found. Install with: cargo install cargo-ndk" >&2
  exit 1
fi

PROFILE_FLAG="--release"
PROFILE_DIR="release"
if [[ "$PROFILE" == "debug" ]]; then
  PROFILE_FLAG=""
  PROFILE_DIR="debug"
fi

echo ">> building Android ABIs (${ABIS[*]}) profile=$PROFILE api=$ANDROID_API"
rm -rf "$OUT_DIR"
mkdir -p "$OUT_DIR/jniLibs" "$OUT_DIR/kotlin"

# cargo-ndk builds every requested ABI and copies the .so into the
# per-ABI jniLibs subdir for us.
ndk_targets=()
for abi in "${ABIS[@]}"; do
  ndk_targets+=(-t "$abi")
done
cargo ndk "${ndk_targets[@]}" -o "$OUT_DIR/jniLibs" --platform "$ANDROID_API" \
  build $PROFILE_FLAG

# --- generate Kotlin bindings ---------------------------------------------
# Library mode reads the FFI contract straight out of a compiled cdylib,
# so the bindings can never drift from the symbols in the shipped .so.
# Any ABI's lib carries identical metadata; use arm64.
BINDGEN_LIB="target/${ABI_TRIPLE[arm64-v8a]}/$PROFILE_DIR/$LIB_NAME"
if [[ ! -f "$BINDGEN_LIB" ]]; then
  echo "error: expected built library not found at $BINDGEN_LIB" >&2
  exit 1
fi
echo ">> generating Kotlin bindings from $BINDGEN_LIB"
cargo run --quiet --bin uniffi-bindgen -- generate \
  --library "$BINDGEN_LIB" \
  --language kotlin \
  --out-dir "$OUT_DIR/kotlin"

# --- strip the shipped jniLibs --------------------------------------------
# The crate is built UNSTRIPPED on purpose so library-mode bindgen (above)
# can read the UniFFI metadata symbols out of the cdylib. Now that the
# Kotlin bindings are generated, strip the .so copies we actually ship so
# the APK stays small — the runtime FFI entry points are exported
# dynamic symbols and survive `--strip-unneeded`.
# NB: llvm-strip is shipped as a symlink to llvm-objcopy in the NDK, so
# match files AND symlinks (no -type f).
LLVM_STRIP="$(find "$ANDROID_NDK_HOME/toolchains/llvm/prebuilt" -name 'llvm-strip' 2>/dev/null | head -n1)"
if [[ -n "$LLVM_STRIP" ]]; then
  echo ">> stripping shipped jniLibs with $LLVM_STRIP"
  find "$OUT_DIR/jniLibs" -name "$LIB_NAME" -print0 | while IFS= read -r -d '' so; do
    "$LLVM_STRIP" --strip-unneeded "$so"
  done
else
  echo ">> warning: llvm-strip not found under NDK; shipping unstripped jniLibs" >&2
fi

echo ">> done. artifacts in $OUT_DIR"
find "$OUT_DIR" -type f | sort
