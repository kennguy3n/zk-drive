//! Version-pinned `uniffi-bindgen` entry point.
//!
//! Running binding generation through this binary (rather than a
//! separately-installed `uniffi-bindgen` crate) guarantees the
//! generator is byte-for-byte the same uniffi version the cdylib /
//! staticlib was compiled against. A mismatch silently produces
//! Swift / Kotlin that fails to link against the contract metadata
//! baked into the library, so the pin is load-bearing — see
//! build-ios.sh / build-android.sh, which both invoke
//! `cargo run --bin uniffi-bindgen -- generate --library ...`.
fn main() {
    uniffi::uniffi_bindgen_main()
}
