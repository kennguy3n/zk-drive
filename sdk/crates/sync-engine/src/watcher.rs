//! Filesystem watcher that emits coalesced [`crate::LocalEvent`]s.
//!
//! Editors typically save by writing to a temp file, renaming it
//! into place, and on some platforms updating mtime twice. The
//! watcher coalesces a burst of events on the same path into a
//! single Upsert / Delete / Rename so the engine doesn't double-
//! upload.

use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc as std_mpsc;
use std::sync::Arc;
use std::thread::JoinHandle;
use std::time::{Duration, Instant};

use notify::event::ModifyKind;
use notify::{Event, EventKind, RecursiveMode, Watcher as NotifyWatcher};
use tokio::sync::mpsc;

use crate::events::LocalEvent;
use crate::hash::content_hash;
use crate::Result;

/// Default coalesce window. Empirically large enough to merge most
/// editor save patterns while still surfacing user-visible edits
/// quickly. Re-exported via the crate root so the CLI / Tauri
/// shell can pass the same value the unit tests run against.
#[allow(dead_code)]
pub const DEFAULT_COALESCE: Duration = Duration::from_millis(250);

/// Spawn a watcher that emits [`LocalEvent`]s on the returned
/// channel. The watcher runs on its own thread (via `notify`) plus a
/// dedicated `std::thread` for coalescing; both are joined when the
/// returned [`Watcher`] is dropped.
///
/// Implementation note: an earlier revision dispatched the
/// coalescing loop to `tokio::task::spawn_blocking`. On Linux that
/// path silently swallowed notify events (the closure's `Receiver`
/// observed `RecvTimeoutError::Timeout` even after `notify` had
/// already enqueued frames) — we suspect cross-thread send/recv
/// ordering quirks against the blocking-pool scheduler. Using a
/// dedicated OS thread sidesteps the issue entirely and the loop
/// is plain blocking I/O anyway, so we get nothing back from
/// running it as a task.
pub struct Watcher {
    _notify: notify::RecommendedWatcher,
    shutdown: Arc<AtomicBool>,
    _join: Option<JoinHandle<()>>,
}

impl Drop for Watcher {
    fn drop(&mut self) {
        self.shutdown.store(true, Ordering::SeqCst);
        if let Some(j) = self._join.take() {
            let _ = j.join();
        }
    }
}

impl Watcher {
    /// Watches `root` recursively and pushes coalesced events to
    /// `tx`. Channel capacity should be sized for the worst-case
    /// burst (a `cp -r` of a few thousand files); 1024 is typically
    /// enough.
    ///
    /// Equivalent to [`Watcher::start_with_ignore`] with an empty
    /// ignore set. Most callers want the ignore-aware constructor
    /// because the engine materialises catalogue stubs under
    /// `<root>/.zk-pending/` and those writes must not bounce back
    /// into the watcher as spurious local events.
    pub fn start(
        root: impl AsRef<Path>,
        coalesce: Duration,
        tx: mpsc::Sender<LocalEvent>,
    ) -> Result<Self> {
        Self::start_with_ignore(root, coalesce, Vec::<PathBuf>::new(), tx)
    }

    /// Same as [`Watcher::start`] but drops any event whose first
    /// path component lies under one of `ignore_prefixes`. Used by
    /// the CLI / Tauri shell to suppress writes the engine itself
    /// performs into the placeholder directory ([`crate::placeholder_dir`]).
    pub fn start_with_ignore<P: Into<PathBuf>>(
        root: impl AsRef<Path>,
        coalesce: Duration,
        ignore_prefixes: impl IntoIterator<Item = P>,
        tx: mpsc::Sender<LocalEvent>,
    ) -> Result<Self> {
        let (raw_tx, raw_rx) = std_mpsc::channel::<notify::Result<Event>>();
        let mut nw = notify::recommended_watcher(move |res: notify::Result<Event>| {
            let _ = raw_tx.send(res);
        })?;
        nw.watch(root.as_ref(), RecursiveMode::Recursive)?;

        let ignore_prefixes: Vec<PathBuf> = ignore_prefixes.into_iter().map(Into::into).collect();
        let shutdown = Arc::new(AtomicBool::new(false));
        let shutdown_thread = shutdown.clone();
        let join = std::thread::Builder::new()
            .name("zk-sync-watcher".into())
            .spawn(move || {
                let mut pending: Vec<(PathBuf, Instant, EventKind)> = Vec::new();
                loop {
                    if shutdown_thread.load(Ordering::SeqCst) {
                        flush(&mut pending, &tx);
                        return;
                    }
                    let next = match raw_rx.recv_timeout(Duration::from_millis(250)) {
                        Ok(ev) => ev,
                        Err(std_mpsc::RecvTimeoutError::Timeout) => {
                            flush(&mut pending, &tx);
                            continue;
                        }
                        Err(std_mpsc::RecvTimeoutError::Disconnected) => {
                            flush(&mut pending, &tx);
                            return;
                        }
                    };
                    if let Ok(ev) = next {
                        push(&mut pending, ev, &ignore_prefixes);
                    }
                    let deadline = Instant::now() + coalesce;
                    while let Ok(ev) =
                        raw_rx.recv_timeout(deadline.saturating_duration_since(Instant::now()))
                    {
                        if let Ok(ev) = ev {
                            push(&mut pending, ev, &ignore_prefixes);
                        }
                    }
                    flush(&mut pending, &tx);
                }
            })
            .map_err(|e| crate::SyncError::Other(format!("spawn watcher thread: {e}")))?;

        Ok(Self {
            _notify: nw,
            shutdown,
            _join: Some(join),
        })
    }
}

/// Returns true if `path` lies inside any of the configured ignore
/// prefixes. Comparison is done on the canonical components so a
/// stray trailing separator doesn't change the answer.
fn is_ignored(path: &Path, prefixes: &[PathBuf]) -> bool {
    prefixes.iter().any(|p| path.starts_with(p))
}

/// Returns true if `kind` is a state-change we care about. Read-only
/// `Access` events are dropped — without this filter a save burst
/// would coalesce to the trailing `Access(Close(Write))` and produce
/// no upsert event.
fn is_actionable(kind: &EventKind) -> bool {
    matches!(
        kind,
        EventKind::Create(_) | EventKind::Modify(_) | EventKind::Remove(_) | EventKind::Other,
    )
}

fn push(pending: &mut Vec<(PathBuf, Instant, EventKind)>, ev: Event, ignore_prefixes: &[PathBuf]) {
    if !is_actionable(&ev.kind) {
        return;
    }
    for path in ev.paths {
        if is_ignored(&path, ignore_prefixes) {
            continue;
        }
        // Keep only the most recent (path, kind) within the window --
        // earlier entries on the same path are superseded.
        pending.retain(|(p, _, _)| p != &path);
        pending.push((path, Instant::now(), ev.kind));
    }
}

fn flush(pending: &mut Vec<(PathBuf, Instant, EventKind)>, tx: &mpsc::Sender<LocalEvent>) {
    for (path, _, kind) in pending.drain(..) {
        let evt = match kind {
            EventKind::Remove(_) => Some(LocalEvent::Delete { path: path.clone() }),
            // Treat Modify(Name) as a rename — but notify reports
            // the destination only, so without a "From" path we
            // surface as Upsert. A true rename is handled by the
            // catalogue when it sees the missing original.
            //
            // Failure to open the file or to hash its contents
            // drops the event on the floor on purpose. The dropped
            // cases are:
            //
            //   * `File::open` fails because the file was deleted
            //     between `notify` firing and the coalesce window
            //     expiring (a rapid create-then-delete burst from
            //     an editor's temp-file write or `rm`).
            //   * `content_hash` fails because the file was
            //     truncated / replaced mid-read.
            //
            // These would race the engine's catalogue regardless of
            // whether we surface them, so silent drop is the safe
            // option: the next reconciliation cycle (next save, or
            // a future `Engine::rescan`) catches up the catalogue
            // state. We do not propagate IO errors to the engine
            // because the watcher thread has no recovery path -- the
            // channel would just receive a noisy `Delete` it can't
            // tell apart from a real deletion.
            EventKind::Modify(ref mk) => match std::fs::File::open(&path) {
                Ok(f) => {
                    let size = f.metadata().ok().map(|m| m.len()).unwrap_or(0);
                    match content_hash(f) {
                        Ok(hash) => Some(LocalEvent::Upsert {
                            path: path.clone(),
                            size_bytes: size,
                            content_hash: hash,
                        }),
                        Err(_) => None,
                    }
                }
                Err(err) => {
                    // A `Modify(Name(_))` whose source path no longer
                    // exists is the strongest signal `notify` gives
                    // us that a rename moved the file away from this
                    // path. Surface it as a `Delete` so the
                    // catalogue's stale row is reconciled now rather
                    // than waiting for `Engine::rescan`. Only emit
                    // for `NotFound` (not e.g. PermissionDenied)
                    // because the latter doesn't imply absence and
                    // would create spurious deletes for permission
                    // flips. Other `Modify(_)` variants
                    // (`Modify(Data(...))`, `Modify(Metadata(...))`,
                    // ...) keep their pre-existing silent-drop
                    // behaviour: a transient open failure on a
                    // content edit is far more likely than a real
                    // rename, and we don't want to mis-translate it
                    // into a Delete.
                    if matches!(mk, ModifyKind::Name(_))
                        && err.kind() == std::io::ErrorKind::NotFound
                    {
                        Some(LocalEvent::Delete { path: path.clone() })
                    } else {
                        None
                    }
                }
            },
            EventKind::Create(_) | EventKind::Other => match std::fs::File::open(&path) {
                Ok(f) => {
                    let size = f.metadata().ok().map(|m| m.len()).unwrap_or(0);
                    match content_hash(f) {
                        Ok(hash) => Some(LocalEvent::Upsert {
                            path: path.clone(),
                            size_bytes: size,
                            content_hash: hash,
                        }),
                        Err(_) => None,
                    }
                }
                Err(_) => None,
            },
            _ => None,
        };
        if let Some(e) = evt {
            // Channel send errors mean the consumer dropped; we stop
            // emitting (the watcher thread will exit on shutdown or
            // on the next disconnected recv). `blocking_send` is
            // safe here because we are not on a tokio worker.
            let _ = tx.blocking_send(e);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn detects_create_and_modify() {
        let dir = tempfile::tempdir().unwrap();
        let (tx, mut rx) = mpsc::channel::<LocalEvent>(32);
        let _w = Watcher::start(dir.path(), Duration::from_millis(100), tx).unwrap();

        // Give the watcher a moment to install.
        tokio::time::sleep(Duration::from_millis(150)).await;

        let p = dir.path().join("hello.txt");
        {
            let mut f = std::fs::File::create(&p).unwrap();
            f.write_all(b"hi").unwrap();
        }

        // Coalesce window + a generous slack.
        let deadline = tokio::time::Instant::now() + Duration::from_secs(3);
        let mut saw_upsert = false;
        while tokio::time::Instant::now() < deadline {
            tokio::select! {
                Some(ev) = rx.recv() => {
                    if let LocalEvent::Upsert { path, .. } = ev {
                        if path == p {
                            saw_upsert = true;
                            break;
                        }
                    }
                }
                _ = tokio::time::sleep(Duration::from_millis(100)) => {}
            }
        }
        assert!(saw_upsert, "watcher did not surface the create");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn ignore_prefix_suppresses_events_under_placeholder_dir() {
        // Regression: a future PR that materialises stub files under
        // `<root>/.zk-pending/<uuid>` must not bounce those writes
        // back into the engine as `LocalEvent::Upsert`.
        let dir = tempfile::tempdir().unwrap();
        let placeholder = dir.path().join(".zk-pending");
        std::fs::create_dir_all(&placeholder).unwrap();

        let (tx, mut rx) = mpsc::channel::<LocalEvent>(32);
        let _w = Watcher::start_with_ignore(
            dir.path(),
            Duration::from_millis(100),
            vec![placeholder.clone()],
            tx,
        )
        .unwrap();
        tokio::time::sleep(Duration::from_millis(150)).await;

        let stub = placeholder.join("00000000-0000-0000-0000-000000000001");
        let real = dir.path().join("real.txt");
        {
            let mut f = std::fs::File::create(&stub).unwrap();
            f.write_all(b"stub").unwrap();
        }
        {
            let mut f = std::fs::File::create(&real).unwrap();
            f.write_all(b"real").unwrap();
        }

        let deadline = tokio::time::Instant::now() + Duration::from_secs(3);
        let mut saw_real = false;
        let mut saw_stub = false;
        while tokio::time::Instant::now() < deadline {
            tokio::select! {
                Some(ev) = rx.recv() => {
                    if let LocalEvent::Upsert { path, .. } = ev {
                        if path == stub { saw_stub = true; }
                        if path == real { saw_real = true; }
                    }
                }
                _ = tokio::time::sleep(Duration::from_millis(100)) => {}
            }
        }
        assert!(saw_real, "watcher must still surface real files");
        assert!(!saw_stub, "watcher must drop events under placeholder dir");
    }

    /// `flush` must surface a `Delete` when it sees a
    /// `Modify(Name(_))` whose source path no longer exists. This is
    /// the strongest signal `notify` gives us that a rename moved
    /// the file away; without this the catalogue row would stay
    /// stale until the next reconciliation pass.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn modify_name_emits_delete_when_source_missing() {
        use notify::event::{ModifyKind, RenameMode};

        let dir = tempfile::tempdir().unwrap();
        let missing = dir.path().join("renamed-away.txt");
        // Path does not exist on disk; mimic notify's
        // Modify(Name(From)) event for a file that has just moved.
        let kind = EventKind::Modify(ModifyKind::Name(RenameMode::From));
        let pending = vec![(missing.clone(), Instant::now(), kind)];

        let (tx, mut rx) = mpsc::channel::<LocalEvent>(4);
        // `flush` uses `blocking_send` which would panic if invoked
        // directly on a tokio worker; route it through
        // `spawn_blocking`, mirroring how the watcher thread itself
        // sits on a dedicated OS thread.
        let missing_for_assert = missing.clone();
        tokio::task::spawn_blocking(move || {
            let mut pending = pending;
            flush(&mut pending, &tx);
            assert!(pending.is_empty(), "flush must drain the pending vec");
            missing_for_assert
        })
        .await
        .unwrap();
        let ev = rx.recv().await.expect("flush must emit one event");
        match ev {
            LocalEvent::Delete { path } => assert_eq!(path, missing),
            other => panic!("expected Delete, got {other:?}"),
        }
    }

    /// A `Modify(Data(_))` event whose path can't be opened must
    /// still be silently dropped -- it is far more likely a transient
    /// open failure on a content edit than a real rename.
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn modify_data_drops_silently_when_open_fails() {
        use notify::event::{DataChange, ModifyKind};

        let dir = tempfile::tempdir().unwrap();
        let missing = dir.path().join("not-renamed.txt");
        let kind = EventKind::Modify(ModifyKind::Data(DataChange::Content));
        let pending = vec![(missing.clone(), Instant::now(), kind)];

        let (tx, mut rx) = mpsc::channel::<LocalEvent>(4);
        tokio::task::spawn_blocking(move || {
            let mut pending = pending;
            flush(&mut pending, &tx);
        })
        .await
        .unwrap();
        let observed = tokio::time::timeout(Duration::from_millis(50), rx.recv())
            .await
            .ok();
        // `Some(None)` would mean the channel closed while empty
        // (sender dropped at end of spawn_blocking, no event sent);
        // `None` would mean the timeout elapsed without a recv. Both
        // satisfy 'no event emitted'.
        match observed {
            None | Some(None) => {}
            Some(Some(ev)) => {
                panic!("Modify(Data(_)) with a missing path must not emit any event, got {ev:?}")
            }
        }
    }
}
