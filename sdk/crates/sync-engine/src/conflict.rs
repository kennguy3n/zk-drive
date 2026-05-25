//! Conflict resolution policies.
//!
//! Default policy is last-writer-wins with a `.conflict.<unix>` side
//! copy preserved on disk. A future policy implementation could
//! offer "manual" or "always-prefer-remote" semantics.

use std::path::{Path, PathBuf};

/// Decision returned by [`ConflictPolicy::resolve`].
#[derive(Debug, Clone)]
pub enum Resolution {
    /// Keep the local copy; reject the incoming remote version.
    PreferLocal,
    /// Apply the remote version. The local copy was saved aside at
    /// `side_copy` (None when there was no diverging local content
    /// to preserve).
    PreferRemote { side_copy: Option<PathBuf> },
}

/// A conflict policy decides what to do when both sides changed
/// since the last reconciled state.
pub trait ConflictPolicy: Send + Sync {
    fn resolve(
        &self,
        local_path: &Path,
        local_hash: &[u8; 32],
        remote_hash: Option<&[u8; 32]>,
    ) -> std::io::Result<Resolution>;
}

/// Default last-writer-wins policy. Preserves the local copy with a
/// `.conflict.<unix_seconds>` suffix before yielding to the remote
/// version. If the local hash matches the remote hash there is no
/// real conflict and the resolution is `PreferRemote { side_copy:
/// None }`.
#[derive(Debug, Default, Clone)]
pub struct LastWriterWins;

impl ConflictPolicy for LastWriterWins {
    fn resolve(
        &self,
        local_path: &Path,
        local_hash: &[u8; 32],
        remote_hash: Option<&[u8; 32]>,
    ) -> std::io::Result<Resolution> {
        if let Some(rh) = remote_hash {
            if rh == local_hash {
                return Ok(Resolution::PreferRemote { side_copy: None });
            }
        }
        if !local_path.exists() {
            return Ok(Resolution::PreferRemote { side_copy: None });
        }
        let now = chrono::Utc::now().timestamp();
        let mut side = local_path.to_path_buf();
        let new_name = match local_path.file_name() {
            Some(n) => format!("{}.conflict.{now}", n.to_string_lossy()),
            None => format!("conflict.{now}"),
        };
        side.set_file_name(new_name);
        std::fs::copy(local_path, &side)?;
        Ok(Resolution::PreferRemote {
            side_copy: Some(side),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;

    #[test]
    fn matching_hashes_produce_no_side_copy() {
        let dir = tempfile::tempdir().unwrap();
        let p = dir.path().join("a.txt");
        {
            let mut f = std::fs::File::create(&p).unwrap();
            f.write_all(b"x").unwrap();
        }
        let h = [9u8; 32];
        let resolution = LastWriterWins.resolve(&p, &h, Some(&h)).unwrap();
        assert!(matches!(
            resolution,
            Resolution::PreferRemote { side_copy: None }
        ));
        // Original still intact.
        assert!(p.exists());
    }

    #[test]
    fn differing_hashes_produce_side_copy_and_keep_original() {
        let dir = tempfile::tempdir().unwrap();
        let p = dir.path().join("a.txt");
        {
            let mut f = std::fs::File::create(&p).unwrap();
            f.write_all(b"local body").unwrap();
        }
        let local_hash = [1u8; 32];
        let remote_hash = [2u8; 32];
        let resolution = LastWriterWins
            .resolve(&p, &local_hash, Some(&remote_hash))
            .unwrap();
        match resolution {
            Resolution::PreferRemote {
                side_copy: Some(side),
            } => {
                assert!(side.exists());
                assert!(side
                    .file_name()
                    .unwrap()
                    .to_string_lossy()
                    .contains(".conflict."));
            }
            other => panic!("unexpected: {other:?}"),
        }
    }
}
