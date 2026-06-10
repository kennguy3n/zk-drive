#!/usr/bin/env bash
#
# build-ios.sh — cross-compile the ZK Drive mobile bridge for iOS and
# assemble an XCFramework with generated Swift bindings.
#
# Output layout (under $OUT_DIR, default ./build/ios):
#   ZkMobileBridge.xcframework/    (device arm64 + fat simulator slice)
#   Sources/ZkMobileBridge/zk_mobile_bridge.swift   (generated Swift API)
#
# The iOS app links the XCFramework and adds the generated Swift file to
# its target. The bindings come from the SAME crate metadata as the
# Kotlin bindings produced by build-android.sh.
#
# IMPORTANT: cross-compiling to the Apple targets requires the Apple SDK
# (xcrun / clang for the iOS sysroot — some deps such as `ring` shell out
# to it from their build scripts), and the final `lipo` +
# `xcodebuild -create-xcframework` link steps require Xcode. Both only
# exist on macOS, so the FULL iOS build (per-arch staticlibs +
# XCFramework) only completes on a macOS runner.
#
# Swift binding generation, however, is target-independent: UniFFI reads
# the contract from the crate metadata, which is identical in any
# compiled artifact. So on a NON-macOS host this script still generates
# the Swift bindings (from a host-built library) and then stops with a
# clear message before the macOS-only stages, rather than failing
# cryptically inside the iOS cross-compile.
#
# Requirements (macOS):
#   * Xcode + command line tools (xcodebuild, lipo)
#   * Rust toolchain with the iOS targets installed:
#       rustup target add aarch64-apple-ios aarch64-apple-ios-sim x86_64-apple-ios
#
# Env knobs:
#   OUT_DIR    output root (default ./build/ios)
#   PROFILE    release|debug (default release)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

OUT_DIR="${OUT_DIR:-$SCRIPT_DIR/build/ios}"
PROFILE="${PROFILE:-release}"
LIB_NAME="libzk_mobile_bridge.a"          # staticlib for XCFramework linking
FRAMEWORK_NAME="ZkMobileBridge"

DEVICE_TARGET="aarch64-apple-ios"
SIM_TARGETS=(aarch64-apple-ios-sim x86_64-apple-ios)
ALL_TARGETS=("$DEVICE_TARGET" "${SIM_TARGETS[@]}")

PROFILE_FLAG="--release"
PROFILE_DIR="release"
if [[ "$PROFILE" == "debug" ]]; then
  PROFILE_FLAG=""
  PROFILE_DIR="debug"
fi

IS_MACOS=false
[[ "$(uname -s)" == "Darwin" ]] && IS_MACOS=true

rm -rf "$OUT_DIR"
mkdir -p "$OUT_DIR/Sources/$FRAMEWORK_NAME" "$OUT_DIR/Headers"

# --- cross-compile the staticlib for every iOS arch -----------------------
# Only on macOS: the Apple-target cargo build needs the iOS SDK (xcrun).
if $IS_MACOS; then
  echo ">> building iOS targets (${ALL_TARGETS[*]}) profile=$PROFILE"
  for target in "${ALL_TARGETS[@]}"; do
    if ! rustup target list --installed | grep -qx "$target"; then
      echo ">> installing rust target $target"
      rustup target add "$target"
    fi
    echo ">> cargo build --target $target"
    cargo build $PROFILE_FLAG --target "$target"
  done
  # Pin the Swift contract to the actual iOS device staticlib.
  BINDGEN_LIB="target/$DEVICE_TARGET/$PROFILE_DIR/$LIB_NAME"
else
  # Off macOS we can't cross-compile the Apple targets, but UniFFI
  # metadata is target-independent, so generate the Swift bindings from a
  # host-built library. Try an existing host cdylib/staticlib first to
  # avoid a redundant rebuild, else build one.
  echo ">> non-macOS host ($(uname -s)): skipping iOS cross-compile (needs the Apple SDK)"
  HOST_CDYLIB_SO="target/$PROFILE_DIR/libzk_mobile_bridge.so"
  HOST_CDYLIB_DYLIB="target/$PROFILE_DIR/libzk_mobile_bridge.dylib"
  HOST_STATICLIB="target/$PROFILE_DIR/$LIB_NAME"
  BINDGEN_LIB=""
  for cand in "$HOST_CDYLIB_SO" "$HOST_CDYLIB_DYLIB" "$HOST_STATICLIB"; do
    [[ -f "$cand" ]] && BINDGEN_LIB="$cand" && break
  done
  if [[ -z "$BINDGEN_LIB" ]]; then
    echo ">> building host library for binding generation"
    cargo build $PROFILE_FLAG
    for cand in "$HOST_CDYLIB_SO" "$HOST_CDYLIB_DYLIB" "$HOST_STATICLIB"; do
      [[ -f "$cand" ]] && BINDGEN_LIB="$cand" && break
    done
  fi
fi

# --- generate Swift bindings ----------------------------------------------
# Library mode keeps the Swift contract pinned to the compiled symbols.
if [[ -z "$BINDGEN_LIB" || ! -f "$BINDGEN_LIB" ]]; then
  echo "error: no library available for Swift binding generation" >&2
  exit 1
fi
echo ">> generating Swift bindings from $BINDGEN_LIB"
GEN_DIR="$(mktemp -d)"
trap 'rm -rf "$GEN_DIR"' EXIT
cargo run --quiet --bin uniffi-bindgen -- generate \
  --library "$BINDGEN_LIB" \
  --language swift \
  --out-dir "$GEN_DIR"

# uniffi emits: <ns>.swift, <ns>FFI.h, <ns>FFI.modulemap. The XCFramework
# headers dir needs the header plus a `module.modulemap` so Swift can
# import the C shim as a Clang module.
cp "$GEN_DIR"/*.swift "$OUT_DIR/Sources/$FRAMEWORK_NAME/"
cp "$GEN_DIR"/*FFI.h "$OUT_DIR/Headers/"
cp "$GEN_DIR"/*FFI.modulemap "$OUT_DIR/Headers/module.modulemap"

# --- macOS-only: assemble the fat simulator lib + XCFramework -------------
if ! $IS_MACOS; then
  cat >&2 <<EOF

>> Swift bindings generated at $OUT_DIR/Sources/$FRAMEWORK_NAME/.
>> Skipping the iOS cross-compile + XCFramework assembly: those stages
>> need macOS (the Apple SDK / xcrun for the iOS targets, and lipo +
>> xcodebuild for the framework) and the host is "$(uname -s)". Re-run
>> this script on a macOS runner (see
>> .github/workflows/mobile-bridge.yml) to produce
>> $FRAMEWORK_NAME.xcframework.
EOF
  exit 0
fi

# An XCFramework can hold at most one library per (platform, arch-set)
# slice, so the two simulator arches must be fused into a single fat
# archive with lipo before they go in.
SIM_FAT="$OUT_DIR/sim/$LIB_NAME"
mkdir -p "$OUT_DIR/sim"
SIM_INPUTS=()
for t in "${SIM_TARGETS[@]}"; do
  SIM_INPUTS+=("target/$t/$PROFILE_DIR/$LIB_NAME")
done
echo ">> lipo simulator slices -> $SIM_FAT"
lipo -create "${SIM_INPUTS[@]}" -output "$SIM_FAT"

echo ">> create-xcframework -> $OUT_DIR/$FRAMEWORK_NAME.xcframework"
rm -rf "$OUT_DIR/$FRAMEWORK_NAME.xcframework"
xcodebuild -create-xcframework \
  -library "target/$DEVICE_TARGET/$PROFILE_DIR/$LIB_NAME" -headers "$OUT_DIR/Headers" \
  -library "$SIM_FAT" -headers "$OUT_DIR/Headers" \
  -output "$OUT_DIR/$FRAMEWORK_NAME.xcframework"

echo ">> done. artifacts in $OUT_DIR"
find "$OUT_DIR/$FRAMEWORK_NAME.xcframework" -maxdepth 2 -type d | sort
