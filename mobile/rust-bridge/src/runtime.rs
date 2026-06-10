//! Shared async runtime backing the bridge's blocking FFI surface.
//!
//! # Why blocking, not async-FFI
//!
//! UniFFI can export `async fn` directly, but that requires each host
//! platform to drive a foreign async executor and complicates the
//! Swift / Kotlin call sites. Mobile network + crypto work already
//! runs off the main thread on both platforms — iOS `BGAppRefreshTask`
//! / a background `DispatchQueue`, Android `WorkManager` / a `Dispatchers.IO`
//! coroutine — so a *blocking* FFI that internally drives async work on
//! a shared runtime is the simpler, stabler contract. Every exported
//! method that does I/O documents that it blocks and must not be called
//! on the UI thread.
//!
//! # One runtime, process-wide
//!
//! All bridge objects share a single multi-threaded runtime created on
//! first use. A mobile process holds at most a handful of bridge
//! objects (one per signed-in workspace), and they all issue the same
//! kind of bursty network I/O, so a shared pool right-sizes thread
//! usage far better than a runtime per object would. The runtime lives
//! for the process lifetime by design — there is no clean "shut down
//! the whole bridge" moment on mobile short of process death.

use std::future::Future;
use std::sync::OnceLock;

use tokio::runtime::{Builder, Runtime};

static RUNTIME: OnceLock<Runtime> = OnceLock::new();

/// Returns the process-wide runtime, building it on first call.
///
/// The worker-thread count is capped at 4: the bridge's workload is
/// I/O-bound (HTTPS to the backend, SQLite on a blocking thread), so a
/// large pool would waste memory on a phone without improving
/// throughput. `enable_all` turns on the I/O and timer drivers reqwest
/// and the changefeed backoff depend on.
fn runtime() -> &'static Runtime {
    RUNTIME.get_or_init(|| {
        Builder::new_multi_thread()
            .worker_threads(4)
            .thread_name("zk-bridge")
            .enable_all()
            .build()
            .expect("build tokio runtime for zk-mobile-bridge")
    })
}

/// Drive `fut` to completion on the shared runtime, blocking the
/// calling (foreign) thread until it resolves.
pub(crate) fn block_on<F: Future>(fut: F) -> F::Output {
    runtime().block_on(fut)
}

/// Spawn a detached background task on the shared runtime. Used by the
/// sync engine's continuous-poll loop; the returned handle lets the
/// caller abort it on `stop()`.
pub(crate) fn spawn<F>(fut: F) -> tokio::task::JoinHandle<F::Output>
where
    F: Future + Send + 'static,
    F::Output: Send + 'static,
{
    runtime().spawn(fut)
}
