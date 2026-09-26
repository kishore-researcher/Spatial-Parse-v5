mod chunker;
mod column_layout;
mod daemon;
mod fidelity;
mod font_metrics;
mod markdown_render;
mod pdf_extract;
mod spatial_filter;
mod standard_font_metrics;
mod table_grid;
mod types;

use serde_json::{json, Value};
use std::io::{self, Read, Write};
use std::time::Instant;
use types::IngestionQuality;

/// One emitted pipeline record, tagged by which output stream it belongs on
/// in the one-shot CLI mode (structured results to stdout, human-readable
/// logs/alerts to stderr). The persistent daemon (Priority 4) merges both
/// into a single NDJSON response instead, wrapping `Log` entries as
/// `{"type":"log","message":...}` -- exactly what the Go orchestrator's
/// subprocess-relay path already does, so a client sees identical output
/// shape regardless of which backend served the request.
pub enum Record {
    Json(Value),
    Log(String),
}

/// Runs the full ingestion pipeline (extraction -> header/footer filtering
/// -> fidelity scoring -> chunking) and returns every emitted record in
/// chronological order. This is the single shared core behind both the
/// one-shot CLI invocation and the persistent Unix-socket daemon -- neither
/// mode duplicates this logic, so a fix or behavior change made here
/// applies identically to both.
pub fn run_pipeline(bytes: &[u8], document_id: &str) -> Result<Vec<Record>, String> {
    let mut records = Vec::new();
    let start = Instant::now();

    let pages = pdf_extract::extract_document(bytes).map_err(|e| format!("failed to parse PDF: {e}"))?;
    let total_pages = pages.len();

    // ---- Pain Point 1, Pass 1: lightweight frequency profiling ----
    let candidates = spatial_filter::pass1_profile_candidates(&pages);
    let clusters = spatial_filter::pass2_build_redact_clusters(&candidates, total_pages);

    let redact_summary: Vec<_> = clusters.iter().filter(|c| c.redact).collect();
    records.push(Record::Log(format!(
        "[pass1/pass2] {} header/footer candidate strings profiled -> {} recurring clusters marked REDACT (>= 30% page frequency)",
        candidates.len(),
        redact_summary.len()
    )));

    // Body font-size baseline computed once across kept (non-redacted)
    // elements, so heading detection is consistent across the whole doc.
    let mut all_kept: Vec<&types::TextElement> = Vec::new();
    let mut kept_per_page: Vec<Vec<&types::TextElement>> = Vec::new();
    for page in &pages {
        let kept: Vec<&types::TextElement> = page
            .elements
            .iter()
            .filter(|e| !spatial_filter::should_redact(e, page.page_height, &clusters))
            .collect();
        all_kept.extend(kept.iter().copied());
        kept_per_page.push(kept);
    }
    let body_font_size = markdown_render::estimate_body_font_size(&all_kept);
    let larger_sizes = markdown_render::rank_larger_sizes(&all_kept, body_font_size);

    // ---- Pain Point 2: render + score fidelity, one page group at a time ----
    let mut markdown_by_page: Vec<(usize, String)> = Vec::new();
    let mut fidelity_reports = Vec::new();
    let mut quarantined_pages = 0usize;
    let mut warning_pages = 0usize;

    for (page, kept) in pages.iter().zip(kept_per_page.iter()) {
        let md = markdown_render::render_page_markdown(kept, body_font_size, &larger_sizes);
        let report = fidelity::score_page(page.page_num, kept, &md, &page.vector_lines);

        // Quarantined pages never reach the chunker (by design -- see Pain
        // Point 2), so their rendered text would otherwise be discarded
        // entirely. Attaching it here is what makes a downstream admin
        // review/approve/edit workflow possible at all: without it, an
        // admin API would have a fidelity *score* to show but no actual
        // content to inspect or correct. Pass/Warning pages omit this
        // field since their text is already available via `chunk` records.
        let fidelity_record = if report.quality == IngestionQuality::Quarantined {
            json!({ "type": "page_fidelity", "report": report, "markdown": md })
        } else {
            json!({ "type": "page_fidelity", "report": report })
        };
        records.push(Record::Json(fidelity_record));

        match report.quality {
            IngestionQuality::Quarantined => {
                quarantined_pages += 1;
                records.push(Record::Log(format!(
                    "[ALERT] page {} quarantined (S_fidelity={:.3} < 0.70) — bypassed vectorization, needs human review",
                    page.page_num, report.s_fidelity
                )));
            }
            IngestionQuality::Warning => {
                warning_pages += 1;
                records.push(Record::Log(format!(
                    "[warn] page {} flagged warning_layout_anomaly (S_fidelity={:.3})",
                    page.page_num, report.s_fidelity
                )));
                markdown_by_page.push((page.page_num, md));
            }
            IngestionQuality::Pass => {
                markdown_by_page.push((page.page_num, md));
            }
        }
        fidelity_reports.push(report);
    }

    // ---- Pain Point 3: AST-based semantic chunking over surviving pages ----
    let chunks = chunker::chunk_document(&markdown_by_page, document_id);
    for chunk in &chunks {
        records.push(Record::Json(json!({ "type": "chunk", "payload": chunk })));
    }

    let elapsed = start.elapsed();
    let avg_fidelity = if !fidelity_reports.is_empty() {
        fidelity_reports.iter().map(|r| r.s_fidelity).sum::<f64>() / fidelity_reports.len() as f64
    } else {
        0.0
    };
    records.push(Record::Json(json!({
        "type": "summary",
        "document_id": document_id,
        "total_pages": total_pages,
        "pages_passed": total_pages - quarantined_pages - warning_pages,
        "pages_warned": warning_pages,
        "pages_quarantined": quarantined_pages,
        "redact_clusters": redact_summary.len(),
        "chunks_emitted": chunks.len(),
        "avg_fidelity": avg_fidelity,
        "elapsed_ms": elapsed.as_millis(),
    })));

    Ok(records)
}

/// Renders records exactly as the daemon's socket response does --
/// `Log` entries wrapped as `{"type":"log","message":...}` JSON, everything
/// else passed through as-is -- so both backends produce byte-for-byte
/// equivalent NDJSON shape for the same document.
pub fn records_to_ndjson(records: &[Record]) -> String {
    records
        .iter()
        .map(|r| match r {
            Record::Json(v) => v.to_string(),
            Record::Log(s) => json!({ "type": "log", "message": s }).to_string(),
        })
        .collect::<Vec<_>>()
        .join("\n")
}

/// Reads the whole PDF into memory once (lopdf requires random access to
/// parse the xref table), but everything *downstream* of that -- filtering,
/// scoring, rendering, chunking -- processes one page group at a time and
/// only ever keeps small, bounded structures (header/footer candidate
/// strings, per-page element lists) in memory simultaneously. See README
/// for the full discussion of why the raw-bytes read is an unavoidable
/// exception to "zero global allocations" for a format like PDF, and how
/// pass1/pass2 are scoped to stay lightweight regardless.
fn main() -> io::Result<()> {
    let args: Vec<String> = std::env::args().collect();

    if args.get(1).map(String::as_str) == Some("--daemon") {
        let socket_path = args.get(2).cloned().unwrap_or_else(|| "/tmp/rag_ingestion_core.sock".to_string());
        return daemon::run(&socket_path);
    }

    let document_id = args.get(2).cloned().unwrap_or_else(|| "document".to_string());
    let bytes = if let Some(path) = args.get(1) {
        std::fs::read(path)?
    } else {
        let mut buf = Vec::new();
        io::stdin().read_to_end(&mut buf)?;
        buf
    };

    match run_pipeline(&bytes, &document_id) {
        Ok(records) => {
            let stdout = io::stdout();
            let mut out = stdout.lock();
            let stderr = io::stderr();
            let mut errout = stderr.lock();
            for record in records {
                match record {
                    Record::Json(v) => writeln!(out, "{v}")?,
                    Record::Log(s) => writeln!(errout, "{s}")?,
                }
            }
            Ok(())
        }
        Err(e) => {
            eprintln!("FATAL: {e}");
            std::process::exit(1);
        }
    }
}
