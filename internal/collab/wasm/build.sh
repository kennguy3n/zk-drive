#!/usr/bin/env bash
# build.sh — rebuild ymerge.wasm from the Rust source in ymerge/.
#
# We commit the compiled .wasm to the repo so the Go build does not
# require a Rust toolchain or network access at CI time — wazero
# loads the binary via `go:embed` and runs it directly. Whenever
# the Rust source changes (or yrs is bumped), re-run this script
# and commit the updated ymerge.wasm alongside the source change.
#
# Requirements:
#   - rustup with the `wasm32-wasip1` target installed (NOT
#     wasm32-unknown-unknown; see the comment block below the
#     `cargo build` invocation for why):
#       rustup target add wasm32-wasip1
#
# Optional (recommended for production):
#   - wasm-opt from binaryen: shrinks the binary by ~30% via
#     additional dead-code elimination beyond what rustc emits.
#     The build script auto-detects and uses it if present.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$HERE/ymerge"

# wasm32-wasip1 (WASI preview1) is preferred over wasm32-unknown-
# unknown because the yrs CRDT transitively depends on getrandom
# for client-ID generation. On wasm32-unknown-unknown getrandom
# emits unresolved imports for the wasm-bindgen runtime (which
# implies a JS host); WASI preview1 instead emits a single
# `random_get` import that wazero's built-in WASI module provides
# natively. No JS shim required.
cargo build --release --target wasm32-wasip1

OUT="target/wasm32-wasip1/release/ymerge.wasm"
DEST="$HERE/ymerge.wasm"

if command -v wasm-opt >/dev/null 2>&1; then
    echo "running wasm-opt -Oz"
    wasm-opt -Oz "$OUT" -o "$DEST"
else
    cp "$OUT" "$DEST"
fi

echo "wrote $DEST ($(stat -c%s "$DEST" 2>/dev/null || stat -f%z "$DEST") bytes)"
