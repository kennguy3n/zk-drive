// ymerge is a tiny Rust crate compiled to `wasm32-unknown-unknown`
// that exposes the two Yjs primitives the Go host needs:
//
//   - `merge_updates`: take N concatenated length-prefixed Yjs
//     update payloads (v1 wire format), apply them in order to a
//     fresh Y.Doc, and return the single compact update that
//     reproduces the same document state via `Doc::transact_mut`
//     + `state.encode_state_as_update_v1(&StateVector::default())`.
//   - `encode_state_vector`: take a single Yjs update payload,
//     apply it to a fresh Y.Doc, and return the state vector
//     (`Doc::transact().state_vector()` → v1 encoding).
//
// Memory model: we expose `alloc` / `dealloc` so the Go host
// (using `wazero`) can write inputs into the wasm linear memory
// and read outputs back. Each call follows the pattern:
//
//   1. Go calls `alloc(input_len)` to get an input pointer.
//   2. Go writes the input bytes into wasm memory at that pointer.
//   3. Go calls `merge_updates(input_ptr, input_len)` (or
//      `encode_state_vector(...)`), which returns a packed
//      `(result_ptr << 32) | result_len` u64. Encoding both
//      pointer and length in a single 64-bit return avoids needing
//      a shared "out-pointer" slot in the wasm linear memory.
//   4. Go reads the output bytes from wasm memory at the result
//      pointer + length.
//   5. Go calls `dealloc(input_ptr, input_len)` AND
//      `dealloc(result_ptr, result_len)` to release the buffers.
//
// Error model: a wasm function CANNOT return Result. We encode
// errors by returning `(0 << 32) | 0` (zero ptr, zero len) — the
// Go host treats a zero pointer as "operation failed" and falls
// back to OpaqueConcatFold for that compaction. The error rate
// is logged via the host's slog so misbehaving payloads surface
// to operators without crashing the server.
//
// Input encoding for `merge_updates`: a sequence of segments
// where each segment is a 4-byte big-endian length followed by
// that many bytes of payload. This matches the existing
// length-prefix bundle format used by `LengthPrefix` /
// `OpaqueConcatFold` so the Go bridge can pass a single
// pre-framed buffer rather than re-encoding to a wasm-specific
// shape.

use std::slice;
use yrs::updates::decoder::Decode;
use yrs::updates::encoder::Encode;
use yrs::{Doc, GetString, ReadTxn, StateVector, Text, Transact, Update};

/// Allocate `size` bytes inside the wasm linear memory. Returns a
/// pointer the Go host can write into. The allocation is owned by
/// the host until it calls `dealloc` with the matching pointer +
/// size; failure to dealloc leaks memory inside the module
/// instance, which is bounded only by the host-configured max
/// memory pages.
#[no_mangle]
pub extern "C" fn alloc(size: usize) -> *mut u8 {
    if size == 0 {
        // Returning a non-null sentinel for zero-byte allocations
        // keeps the host's "ptr == 0 means failure" convention
        // intact for genuine OOM cases. We never read from the
        // returned pointer so any non-null is fine.
        return 1 as *mut u8;
    }
    let mut buf = Vec::<u8>::with_capacity(size);
    let ptr = buf.as_mut_ptr();
    std::mem::forget(buf);
    ptr
}

/// Free a previously-`alloc`'d buffer. `size` must match the size
/// passed to alloc — Rust's Vec layout depends on the original
/// capacity, so a mismatched dealloc is UB. The Go host always
/// remembers the size when it remembers the pointer.
///
/// Safety: the (ptr, size) pair must come from a prior `alloc`
/// call on this same wasm instance.
#[no_mangle]
pub unsafe extern "C" fn dealloc(ptr: *mut u8, size: usize) {
    if size == 0 || ptr.is_null() {
        return;
    }
    // SAFETY: ptr came from alloc above, size matches the
    // original capacity, len=0 is safe because we never wrote
    // through the Vec's len cursor.
    let _ = Vec::from_raw_parts(ptr, 0, size);
}

/// Pack a (ptr, len) pair into a single u64 return value. The Go
/// host unpacks via `result >> 32` and `result & 0xFFFFFFFF`.
#[inline]
fn pack_result(ptr: *const u8, len: usize) -> u64 {
    ((ptr as u64) << 32) | (len as u64 & 0xFFFFFFFF)
}

/// Allocate a new wasm buffer holding the contents of `bytes` and
/// return its (ptr, len) packed for return to the host. Caller
/// must remember to `dealloc` the buffer once it has copied the
/// payload out.
fn return_bytes(bytes: Vec<u8>) -> u64 {
    if bytes.is_empty() {
        // An empty result is legal — e.g. a no-op merge of one
        // update returns the update unchanged, but a merge of
        // zero updates returns an empty state. We return the
        // same non-null sentinel `alloc(0)` would have produced
        // (and which the Go host already special-cases via
        // writeToWasm for zero-byte inputs) instead of taking a
        // real 1-byte allocation. This avoids a per-call leak
        // that the previous implementation produced: the host's
        // deferred `dealloc(ptr, 0)` returns early because size
        // is zero, never reclaiming the 1-byte placeholder the
        // wasm side had handed out.
        return pack_result(1 as *const u8, 0);
    }
    let len = bytes.len();
    let mut boxed = bytes.into_boxed_slice();
    let ptr = boxed.as_mut_ptr();
    std::mem::forget(boxed);
    pack_result(ptr, len)
}

/// Parse a length-prefixed segment stream into individual updates.
/// Each segment is a 4-byte big-endian length followed by that
/// many bytes of payload. Returns None on any framing error
/// (truncated header, declared length exceeds remaining bytes).
fn parse_segments(buf: &[u8]) -> Option<Vec<&[u8]>> {
    let mut out = Vec::new();
    let mut cursor = 0usize;
    while cursor < buf.len() {
        if buf.len() - cursor < 4 {
            return None;
        }
        let len_bytes: [u8; 4] = buf[cursor..cursor + 4].try_into().ok()?;
        let seg_len = u32::from_be_bytes(len_bytes) as usize;
        cursor += 4;
        if buf.len() - cursor < seg_len {
            return None;
        }
        out.push(&buf[cursor..cursor + seg_len]);
        cursor += seg_len;
    }
    Some(out)
}

/// `merge_updates` is the primary entry point used by the Go
/// fold function. It parses a length-prefixed concatenation of
/// Yjs updates, applies each one in turn to a fresh Y.Doc, and
/// returns the compact single-update encoding of the resulting
/// document state.
///
/// The returned update, when applied to a fresh Y.Doc on the
/// client, reproduces a document equivalent to the one obtained
/// by applying the original updates in order — Yjs CRDT
/// semantics guarantee equivalence regardless of merge ordering.
///
/// Error path: a non-parseable input frame, an update that
/// `yrs::Update::decode_v1` rejects, or a transaction-encoding
/// failure return (0, 0). The Go host falls back to
/// OpaqueConcatFold for that compaction.
///
/// # Safety
/// `input_ptr` must point to `input_len` bytes of memory inside
/// the wasm linear memory that the host previously wrote via
/// `alloc` + memory write. The function does NOT free the input;
/// the host is responsible for calling `dealloc(input_ptr,
/// input_len)` after reading the result.
#[no_mangle]
pub unsafe extern "C" fn merge_updates(input_ptr: *const u8, input_len: usize) -> u64 {
    if input_ptr.is_null() {
        return 0;
    }
    let input = slice::from_raw_parts(input_ptr, input_len);
    let segments = match parse_segments(input) {
        Some(s) => s,
        None => return 0,
    };

    let doc = Doc::new();
    {
        let mut txn = doc.transact_mut();
        for seg in segments.iter() {
            // An empty segment is legal in the framing (e.g. an
            // empty initial state) — skip it without invoking
            // the decoder, which rejects zero-byte updates.
            if seg.is_empty() {
                continue;
            }
            let update = match Update::decode_v1(seg) {
                Ok(u) => u,
                Err(_) => return 0,
            };
            if txn.apply_update(update).is_err() {
                return 0;
            }
        }
    }

    let merged = doc
        .transact()
        .encode_state_as_update_v1(&StateVector::default());
    return_bytes(merged)
}

/// `apply_and_extract_text` is a test-only helper exported so the
/// Go test layer can verify a merged update reproduces the
/// expected document content. It applies the input update to a
/// fresh Y.Doc, reads the Y.Text named "t", and returns the UTF-8
/// bytes of the text.
///
/// This is the highest-fidelity correctness assertion we can make
/// from the Go side: the wasm-side yrs is the canonical decoder
/// for v1 updates, so a merge that reproduces the right text is
/// guaranteed to be semantically correct (it doesn't merely
/// "decode as a valid SV" — it actually replays to the same
/// observable document state).
///
/// # Safety
/// Same as `merge_updates` — (ptr, len) must reference a wasm-
/// host-allocated buffer.
#[no_mangle]
pub unsafe extern "C" fn apply_and_extract_text(input_ptr: *const u8, input_len: usize) -> u64 {
    if input_ptr.is_null() {
        return 0;
    }
    let input = slice::from_raw_parts(input_ptr, input_len);
    let doc = Doc::new();
    let text = doc.get_or_insert_text("t");
    if !input.is_empty() {
        let update = match Update::decode_v1(input) {
            Ok(u) => u,
            Err(_) => return 0,
        };
        if doc.transact_mut().apply_update(update).is_err() {
            return 0;
        }
    }
    let s = text.get_string(&doc.transact());
    return_bytes(s.into_bytes())
}

/// `make_text_update` constructs a fresh Y.Doc with the given
/// client_id, inserts the input bytes as text content (UTF-8) into
/// a Y.Text named "t", and returns the v1-encoded update that
/// captures the entire doc state.
///
/// This is exported primarily so the Go test layer can generate
/// real yrs-produced fixtures at test time without depending on
/// the canonical Yjs JS library — the alternative (hand-rolled
/// magic bytes) is fragile across yrs versions because the v1
/// wire format includes integer-varint encodings that the spec
/// allows multiple equivalent representations for.
///
/// The function expects input bytes laid out as:
///   - 8 bytes big-endian: client_id (u64)
///   - remainder: UTF-8 text content to insert
///
/// # Safety
/// Same as `merge_updates` — (ptr, len) must reference a wasm-
/// host-allocated buffer.
#[no_mangle]
pub unsafe extern "C" fn make_text_update(input_ptr: *const u8, input_len: usize) -> u64 {
    if input_ptr.is_null() || input_len < 8 {
        return 0;
    }
    let input = slice::from_raw_parts(input_ptr, input_len);
    let client_id = u64::from_be_bytes(input[..8].try_into().unwrap());
    let content = match std::str::from_utf8(&input[8..]) {
        Ok(s) => s,
        Err(_) => return 0,
    };
    let doc = Doc::with_client_id(client_id);
    let text = doc.get_or_insert_text("t");
    {
        let mut txn = doc.transact_mut();
        text.insert(&mut txn, 0, content);
    }
    let update = doc
        .transact()
        .encode_state_as_update_v1(&StateVector::default());
    return_bytes(update)
}

/// `encode_state_vector` takes a single Yjs update (v1 wire
/// format) and returns the state vector of a fresh Y.Doc that
/// has had that update applied.
///
/// The state vector is the compact summary of "what client/clock
/// pairs this doc has seen" — clients use it to ask peers for
/// the deltas they haven't observed. Returning this alongside the
/// merged update lets the snapshot endpoint hand clients both
/// the document state AND the watermark they can use for an
/// efficient catch-up subscribe.
///
/// # Safety
/// Same contract as `merge_updates`: the (ptr, len) pair must
/// reference host-allocated wasm memory, and the host is
/// responsible for freeing both the input and the result.
#[no_mangle]
pub unsafe extern "C" fn encode_state_vector(input_ptr: *const u8, input_len: usize) -> u64 {
    if input_ptr.is_null() {
        return 0;
    }
    let input = slice::from_raw_parts(input_ptr, input_len);

    let doc = Doc::new();
    if !input.is_empty() {
        let update = match Update::decode_v1(input) {
            Ok(u) => u,
            Err(_) => return 0,
        };
        if doc.transact_mut().apply_update(update).is_err() {
            return 0;
        }
    }
    let sv = doc.transact().state_vector().encode_v1();
    return_bytes(sv)
}
