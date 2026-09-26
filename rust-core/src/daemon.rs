//! Track A Priority 4: a persistent worker daemon, listening on a Unix
//! Domain Socket, that eliminates per-request process-spawn overhead.
//!
//! Design note: this deliberately does NOT pull in an async runtime
//! (tokio, etc). Thread-per-connection over `std::os::unix::net` is
//! simple, needs no new dependencies (this project already fought a real
//! MSRV-pinning battle getting `tokenizers` to compile in this sandbox --
//! see README -- adding an async runtime risks a repeat of that for a
//! workload that doesn't need it: each request is a synchronous,
//! CPU-bound ~100-300ms computation, not a long-lived connection juggling
//! thousands of concurrent idle clients). If throughput under real
//! concurrent load ever demands it, swapping this for an async runtime or
//! a bounded worker-thread pool is a contained change -- `handle_connection`
//! and `run_pipeline` are already decoupled from how connections are
//! accepted.
//!
//! Wire protocol (deliberately simple, symmetric length-prefixed framing):
//!   Request:  [4 bytes BE: len(document_id)][document_id bytes]
//!             [4 bytes BE: len(pdf_bytes)][pdf_bytes]
//!   Response: [4 bytes BE: len(response)][response bytes]
//! A connection can carry many request/response cycles before closing --
//! that persistence is the entire point (the Rust process, and everything
//! it loaded once at startup -- most notably the ~3MB embedded tokenizer
//! vocabulary parsed into a `Tokenizer` behind a `OnceLock` -- stays warm
//! across requests instead of being re-paid on every single document).

use std::io::{Read, Write};
use std::os::unix::net::{UnixListener, UnixStream};
use std::thread;

const MAX_FRAME_BYTES: u32 = 1 << 30; // 1GB guard against a malformed length prefix

pub fn run(socket_path: &str) -> std::io::Result<()> {
    // Clear a stale socket file left behind by a previous crashed run --
    // UnixListener::bind fails with AddrInUse otherwise even though nothing
    // is actually listening anymore.
    if std::path::Path::new(socket_path).exists() {
        std::fs::remove_file(socket_path)?;
    }
    let listener = UnixListener::bind(socket_path)?;
    eprintln!("[daemon] listening on {socket_path}");

    for incoming in listener.incoming() {
        match incoming {
            Ok(stream) => {
                thread::spawn(move || {
                    if let Err(e) = handle_connection(stream) {
                        eprintln!("[daemon] connection error: {e}");
                    }
                });
            }
            Err(e) => eprintln!("[daemon] accept error: {e}"),
        }
    }
    Ok(())
}

fn read_frame(stream: &mut UnixStream) -> std::io::Result<Vec<u8>> {
    let mut len_buf = [0u8; 4];
    stream.read_exact(&mut len_buf)?;
    let len = u32::from_be_bytes(len_buf);
    if len > MAX_FRAME_BYTES {
        return Err(std::io::Error::new(std::io::ErrorKind::InvalidData, format!("frame length {len} exceeds {MAX_FRAME_BYTES} byte guard")));
    }
    let mut buf = vec![0u8; len as usize];
    stream.read_exact(&mut buf)?;
    Ok(buf)
}

fn write_frame(stream: &mut UnixStream, data: &[u8]) -> std::io::Result<()> {
    let len = (data.len() as u32).to_be_bytes();
    stream.write_all(&len)?;
    stream.write_all(data)?;
    stream.flush()
}

/// Serves one client connection until it closes, running the pipeline once
/// per request/response cycle on that same persistent connection -- no
/// process spawn, no re-parsing the embedded tokenizer, just the actual
/// computation.
fn handle_connection(mut stream: UnixStream) -> std::io::Result<()> {
    loop {
        let document_id_bytes = match read_frame(&mut stream) {
            Ok(b) => b,
            Err(e) if e.kind() == std::io::ErrorKind::UnexpectedEof => return Ok(()), // client closed the connection -- normal end of session
            Err(e) => return Err(e),
        };
        let document_id = String::from_utf8_lossy(&document_id_bytes).to_string();
        let pdf_bytes = read_frame(&mut stream)?;

        let response_body = match crate::run_pipeline(&pdf_bytes, &document_id) {
            Ok(records) => crate::records_to_ndjson(&records),
            Err(e) => serde_json::json!({ "type": "fatal_error", "message": e }).to_string(),
        };

        write_frame(&mut stream, response_body.as_bytes())?;
    }
}
