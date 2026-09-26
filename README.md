# RAG Ingestion Pipeline — Deterministic PDF → Markdown Engine

A zero-AI, deterministic preprocessing pipeline for enterprise PDF-to-Markdown
ingestion, built as a Go orchestration layer around a Rust processing core —
plus a multi-tenant admin/observability/export layer (Track C) and a local
storage + embedded dashboard layer (Track D) on top.

**Status: working v1 foundation, not a finished production system.** Every
algorithm described below is real, compiles, and is covered by passing unit
tests (37 Rust + 27 Go) plus end-to-end runs against generated test PDFs —
including a 27-page fixture (27/27 pages processed, 4 header/footer
clusters correctly redacted, 1 garbled page correctly quarantined, a real
ruled table, a genuine 2-column layout, 8 context-aware chunks correctly
emitted), dedicated Form XObject fixtures proving recursive extraction
actually works, and a live Postgres+pgvector instance for real (not
mocked) database integration testing. It is a solid foundation to build
on, not a drop-in replacement for a team spending several weeks hardening
this against the full variety of real-world PDFs and a real production
deployment's auth/ops requirements.

## Quick start

```bash
# 1. Build the Rust core
cd rust-core
cargo build --release
cargo test --release   # 37 unit tests

# 2. Run it directly against a PDF (writes NDJSON to stdout, logs to stderr)
./target/release/rag_ingestion_core /path/to/document.pdf my_document_id

# 3. Or run it behind the Go HTTP orchestrator (subprocess-per-request, the default)
cd ../go-orchestrator
go build -o orchestrator .
RUST_CORE_PATH=../rust-core/target/release/rag_ingestion_core ./orchestrator
# in another shell -- note /ingest requires a tenant API key as of Track C;
# with no TENANT_API_KEYS env var set, the server falls back to a single
# logged dev-mode key, 'dev-key', shown here (never rely on this in production):
curl -X POST http://localhost:8080/ingest \
  -H "X-API-Key: dev-key" \
  -F "file=@/path/to/document.pdf" \
  -F "document_id=my_document_id"

# 4. Or, for lower steady-state latency (Track B Priority 4): start the persistent
#    daemon, then point the orchestrator at its socket.
cd ../rust-core
./target/release/rag_ingestion_core --daemon /tmp/rag_ingestion_core.sock &
cd ../go-orchestrator
RUST_CORE_PATH=../rust-core/target/release/rag_ingestion_core \
  RUST_DAEMON_SOCKET=/tmp/rag_ingestion_core.sock \
  ./orchestrator
# /health reports whether the daemon is actually reachable; if it isn't,
# every request transparently falls back to subprocess mode automatically.

# 5. Track C: review quarantined pages, then export to a vector DB.
curl -H "X-API-Key: dev-key" http://localhost:8080/api/v1/quarantine
curl -X POST -H "X-API-Key: dev-key" -H "Content-Type: application/json" \
  -d '{"reviewed_markdown": "corrected text"}' \
  http://localhost:8080/api/v1/quarantine/{id}/approve
curl -X POST -H "X-API-Key: dev-key" -H "Content-Type: application/json" \
  -d '{"target":"pgvector","pg_dsn":"postgres://...","include_approved_quarantine":true,"document_id":"my_document_id"}' \
  http://localhost:8080/api/v1/export
# Prometheus scrapes http://localhost:8080/metrics (unauthenticated, meant
# for an internal-network scraper, not tenant self-service).

# 6. Track D: open http://localhost:8080/ (or /dashboard) in a browser for
#    the embedded UI -- upload, watch ingestion live, browse documents/
#    chunks, review quarantine, trigger exports, all without curl.
```

The HTTP response is streamed newline-delimited JSON: `page_fidelity` records
as each page is scored, `log` lines for redaction stats and quarantine
alerts, `chunk` records as they're emitted, and one final `summary` record.
This shape is identical whether the request was served by the daemon or a
subprocess.

## Architecture

```
Incoming PDF (multipart upload)
        │
        ▼
Go Orchestrator (net/http, stdlib only)
  - multipart.Reader streams the upload straight to a temp file
    (never buffers the whole file in the Go process's own heap)
  - EITHER dials the persistent Rust daemon over a Unix Domain Socket
    (Track B Priority 4, opt-in via RUST_DAEMON_SOCKET)
  - OR spawns the Rust core as a one-shot subprocess (default),
    piping stdout/stderr back to the HTTP client as NDJSON
        │
        ▼
Rust Processing Core (one-shot CLI, or --daemon socket mode)
  - main.rs                -- shared run_pipeline() core; CLI arg parsing; daemon dispatch
  - daemon.rs               -- Track B Priority 4: persistent Unix socket worker
  - pdf_extract.rs          -- recursive content-stream interpreter (Track B Priority 1:
                               Form XObject recursion) -> positioned text elements,
                               real per-glyph widths, vector line/rect tracking
  - font_metrics.rs         -- Track A Priority 1: /Widths, /W (CID), standard-14 fallback
  - standard_font_metrics.rs -- vendored core-14 font width tables (generated, see below)
  - table_grid.rs           -- Track A Priority 2: ruled-table lattice detection from vector lines
  - column_layout.rs        -- Track A Priority 3: X-axis density clustering for column reading order
  - spatial_filter.rs       -- Pain Point 1: header/footer frequency filter (R-tree + NLD)
  - fidelity.rs             -- Pain Point 2: structural fidelity scoring + quality gates
  - markdown_render.rs      -- turns kept elements into Markdown (column-aware; heading/table heuristics)
  - chunker.rs              -- Pain Point 3: AST-based semantic chunking, real offline BPE tokenizer

Go Orchestrator files (Track C, net/http + one dependency, lib/pq -- see below)
  - main.go          -- routing, upload handling, daemon/subprocess dispatch, per-line NDJSON inspection
  - store.go          -- tenant-isolated persistent quarantine store (Feature 1)
  - auth.go            -- API-key -> tenant authentication middleware
  - admin_api.go        -- GET/POST /api/v1/quarantine* handlers
  - metrics.go           -- hand-rolled Prometheus-format /metrics (Feature 2)
  - tracing.go            -- structured JSON span logging (Feature 2)
  - export.go              -- Chunk/Embedder/Exporter shared interfaces (Feature 3)
  - export_pgvector.go      -- Postgres + pgvector connector (needs lib/pq)
  - export_qdrant.go         -- Qdrant connector, plain REST, zero dependencies
  - export_pinecone.go        -- Pinecone connector, plain REST, zero dependencies
  - export_api.go               -- POST /api/v1/export handler tying the above together
  - docstore.go                   -- Track D: local data/tenants/.../documents/ persistence
  - documents_api.go                -- GET /api/v1/documents, .../chunks, .../markdown
  - dashboard.go                      -- serves the embedded UI via go:embed
  - dashboard.html                      -- the UI itself: single file, no build step
```

### Why subprocess pipes instead of FFI/WASM (and how the daemon fits in)

The original spec's diagram showed "Zero-Copy Memory Stream Transfer" via
FFI/WASM between Go and Rust. We used subprocess + stdin/stdout/stderr
pipes instead, deliberately:

- No cgo toolchain coupling — cgo requires matching ABI/allocator
  assumptions between Go and Rust and complicates cross-compilation.
- Process isolation — a panic or OOM in the Rust core (a 500-page
  worst-case document is exactly the scenario where this matters) can't take
  the Go server down with it.
- It's a straightforward drop-in to swap later for a `cdylib` + cgo binding,
  or for gRPC to a separate Rust worker pool, without changing the
  HTTP-facing contract.

Track B Priority 4 (the persistent daemon) takes exactly that "drop-in"
upgrade path — a Unix Domain Socket instead of a subprocess per request —
while keeping the process-isolation property: the daemon still runs as a
separate OS process from the Go server, so a panic in the Rust core still
can't take Go down with it, and the subprocess path remains available as
an automatic fallback. See the Track B section below for what changed,
what didn't, and measured before/after latency.

## Design corrections made to the literal spec

A few places in the spec, taken completely literally, would have produced a
system that either couldn't work or would silently misbehave. Each is
implemented as the corrected version, not the literal one, and documented
in the corresponding source file:

1. **Pass 1/Pass 2 is not fully "single-pass streaming."** A global
   frequency-ratio redaction algorithm mathematically requires having seen
   every page before you know what to redact on page 1 — you cannot know a
   ratio over N pages having only seen 1. We resolve this by keeping Pass 1
   deliberately lightweight: it extracts only header/footer-zone candidate
   strings (a few hundred short strings for a 500-page document), never full
   page bodies, so the "no global DOM in RAM" spirit holds even though two
   passes over the byte stream are unavoidable for this specific algorithm.
   See `spatial_filter.rs`.

2. **Fidelity scoring must not penalize intentional redaction.** The spec's
   literal `C_raw` ("alphanumeric characters from native PDF binary reading")
   would count header/footer text that Pain Point 1 *correctly* stripped as
   "lost content," flagging good pages as broken. `C_raw` is computed only
   over the non-redacted (kept) elements, so the score measures genuine
   extraction/rendering loss, not deliberate cleanup. See `fidelity.rs`.

3. **A header appearing mid-chunk must force a chunk boundary — but only
   if its text actually changes.** The literal pseudocode only flushes a
   chunk on token overflow, which lets two unrelated sections silently merge
   into one chunk carrying only the *first* section's `parent_hierarchy`
   (verified by a failing test before the fix). The naive fix — flush on
   every header line — instead badly over-fragments documents where the same
   sub-heading is printed on every page of a long section (e.g. a running
   "Appendix Notes" header), turning one section into dozens of near-empty
   chunks. The implemented behavior only starts a new section (and flushes)
   when the header text actually differs from what's already at that
   context depth. See `chunker.rs`.

## Offline tokenizer (Priority 3, implemented)

`chunker::get_exact_token_count` now uses the real `tokenizers` crate with a
cl100k_base-equivalent BPE vocabulary embedded via `include_str!` — no
runtime network calls, no file-path configuration. See
`rust-core/resources/README.md` for exactly where the vocabulary data came
from and how it was verified.

**One correction to the sprint spec's snippet:** the proposed
implementation called `Tokenizer::from_str(TOKENIZER_DATA).unwrap()` *inside*
`get_exact_token_count` itself, meaning every single call would re-parse the
full ~100k-vocab/~100k-merge JSON document from scratch. That's fine once;
it's a serious performance bug called once per AST node across a whole
document. The tokenizer is now parsed once into a `std::sync::OnceLock` and
reused for the process's lifetime — one ~50ms parse instead of paying that
cost per paragraph/table/list node.

**Getting `tokenizers` to compile at all was the bulk of the work here.**
This sandbox's `rustc` is pinned at 1.75.0 (Ubuntu 24.04's packaged
version, and the only one reachable through this environment's network
allowlist — no `rustup`/`static.rust-lang.org` access to install a newer
one). `tokenizers` 0.19's dependency tree has a hard MSRV floor around
rustc 1.80 with no way to pin around it (`rayon` ≥1.10 unconditionally
requires `rayon-core` ≥1.13, which itself needs 1.80+). Downgrading to
`tokenizers = "0.15"` with `default-features = false, features = ["onig"]`
avoids that specific wall, but still required pinning several transitive
dependencies to older, 1.75-compatible versions in `Cargo.lock`:
`monostate` 0.1.12, `rayon` 1.8.1, `rayon-core` 1.12.1,
`unicode-segmentation` 1.11.0, `onig` 6.4.0 (which also downgrades
`bitflags` to 1.3.2). **If you build this on a normal, non-sandboxed
machine with a current stable `rustc` (1.80+), you almost certainly don't
need any of these pins** — feel free to `cargo update` and go back to plain
`tokenizers = "0.19"` with default features once you're not fighting this
sandbox's constraints.

## Track A: The High-Fidelity Layout Layer (implemented)

### Priority 1 — real font widths

`font_metrics.rs` parses a page's actual font resources and resolves, per
font, one of:
1. An explicit `/Widths` array (embedded/subset fonts — the common case for
   anything produced by Word, LaTeX, or a browser's print-to-PDF).
2. A **standard-14 core font fallback** when there's no `/Widths` array at
   all — which turned out to be essential, not optional: our own
   reportlab-generated test fixture references `Helvetica`/`Helvetica-Bold`
   directly with zero embedded width data, exactly like a large fraction of
   real-world "quick report" PDFs. Without this fallback, "parse /Widths"
   would have done nothing for our own test document. The metrics table is
   generated (`tools/generate_standard_font_metrics.py`) from PDF.js's
   published core-font tables (Apache-2.0) and verified against known AFM
   values (Helvetica space=278, 'A'=667, 'a'=556 — all confirmed exact).
3. A `/W` array for composite/CID fonts (both run-length forms from the PDF
   spec), assuming 2-byte Identity-H/V codes — the common case, though not
   the only possible CMap structure (see limitations below).
4. A last-resort flat estimate if none of the above apply.

### Priority 2 — vector graphics operator tracking

`pdf_extract.rs` now tracks `m`/`l`/`re`/`c`/`v`/`y`/`h` path construction
and `S`/`s`/`f`/`F`/`f*`/`B`/`B*`/`b`/`b*`/`n` painting operators, producing
line segments in device space. `table_grid.rs` clusters these into
horizontal/vertical rule positions and detects a genuine lattice (rules
that mutually intersect at least twice, not just any two stray lines
anywhere on the page).

**Correction to the ticket's framing:** ruled-line detection gives
high-confidence, exact grid dimensions *for tables that actually have drawn
borders* — it does not make table detection "100% accurate" in general,
because a very large share of real-world tables (including our own page-2
fixture) have **no drawn borders at all** and rely purely on column
alignment. `fidelity::detect_pdf_grid` therefore tries the ruled-line
detector first and only falls back to the spatial-alignment heuristic when
no ruled grid is found — the two are complements, not one superseding the
other. Verified end-to-end against a real drawn-border table (page 26 of
the test fixture): `t_matrix: 1.0`, ruled grid detected via actual `re`/`l`/`S`
operators, not the heuristic.

### Priority 3 — X-axis column clustering

`column_layout.rs` sweeps a page's elements in fixed-width x-buckets,
looking for a run of buckets that stays empty across most lines (a real
column gutter) rather than the position-varying gaps normal word/sentence
spacing produces. Deliberately conservative — a false-positive column split
is a worse failure mode than occasionally missing a real one, since it
would scramble an ordinary single-column document's reading order.

**A real bug found and fixed during end-to-end testing, not just unit
tests:** the first working version correctly split genuine 2-column prose
into two columns, but also misread our page-2 **borderless 4-column data
table** ("Quarter | Product X | Product Y | Total") as four separate
reading columns — rendering each column of *numbers* top-to-bottom as if it
were a wrapped paragraph, destroying the table's row structure entirely. A
data table's columns and a magazine layout's columns are structurally
identical to a naive X-axis sweep (vertically-aligned content, consistent
gaps) — the fix adds a content-based check: genuine text columns average
several words per line (prose), while table cells average close to one
short value per line. Both the original bug and the fix are captured as
permanent regression tests (`borderless_table_is_not_misread_as_reading_columns`,
`reproduces_real_world_page27_coordinates`). This is exactly the kind of
interaction bug that only surfaces when priorities are tested *together*
against realistic documents rather than in isolation.

**Known limitation:** column detection assumes the whole page follows one
consistent structure. A page mixing a full-width banner/headline with a
2-column body below it isn't specially handled — enough full-width lines
mixed into an otherwise 2-column page can wash out the gutter signal,
degrading gracefully to 1 detected column rather than crashing, but not
recovering the true boundary. Also: a genuine narrow list of short items
(e.g. a numbered list of single words) could, in principle, be rejected by
the words-per-line check the same way the table fix rejects it — a
real, if less common, trade-off of the same fix that solved the table
false-positive.

## Track B: Structural Recursion & Daemonization (implemented)

### Priority 1 — Form XObject lexing recursion

`pdf_extract.rs`'s content-stream interpreter is now a recursive
`Interpreter` struct: when `Do` references a Form XObject, it resolves the
form's `/Resources` (falling back to the caller's, per spec, if the form
doesn't carry its own), computes a nested CTM (`form /Matrix` concatenated
with the CTM at the point of invocation), and recurses into the form's own
content stream — sharing the same page-scoped `elements`/`vector_lines`
accumulators, so a form's contents land in the output already transformed
into top-level page device space, indistinguishable from directly-drawn
content downstream. Passing *copies* of the CTM and text state into the
recursive call (not mutable references) gives the correct implicit-`q...Q`
semantics for free — whatever a form does to its own graphics state can't
leak back to the caller.

Guarded against the well-known PDF-parser DoS vector of self-referencing
XObjects two ways: a hard recursion-depth cap (`MAX_XOBJECT_DEPTH = 12`)
and a `visiting: HashSet<ObjectId>` that refuses to re-enter an XObject
already on the current call stack. Image XObjects (the other legal `Do`
target) are recognized via `/Subtype` and skipped outright — no text or
vectors to extract from raster data.

**Verified, not just implemented** — three reportlab-generated fixtures
(`testdata/xobject_*.pdf`, regenerable via
`testdata/generate_xobject_fixtures.py`) are checked directly against
`pdf_extract::extract_document` as permanent automated tests:
- A single Form XObject hiding an invoice's subtotal/tax/total block —
  confirmed the *literal bug the ticket describes*: before this fix, only
  the page's own direct content ("Invoice INV-2026-0088") was extracted;
  the entire hidden block was invisible. After the fix, all of it comes
  through with correct heading attribution.
- Two levels of nested Form XObjects (a form invoking another form) —
  confirms recursion isn't just one level deep.
- A real Image XObject invoked via `Do` alongside the forms above —
  confirms it's skipped cleanly with no crash and no raster-data leaking
  into extracted text.

**Known limitation:** BBox clipping is approximated (whole
elements/segments are kept or dropped by their center point against the
form's transformed BBox, not true partial clipping of a text run that
straddles the boundary) — adequate for keeping an off-canvas reused
template's content out of the page, which is the failure mode that matters
for RAG ingestion, but not geometrically exact.

### Priority 4 — persistent IPC worker pool

`daemon.rs` adds a `--daemon <socket_path>` mode: a Unix Domain Socket
server, thread-per-connection, serving requests through the *exact same*
`run_pipeline` function the one-shot CLI mode uses (extracted into
`main.rs` specifically so neither mode duplicates pipeline logic). A simple
length-prefixed framing protocol (request: document_id, then PDF bytes;
response: NDJSON) lets one connection carry many requests, which is what
actually eliminates the cold-start cost — not just "the process doesn't
exit," but concretely: the ~3MB embedded tokenizer vocabulary is parsed
into a `Tokenizer` behind a `OnceLock` exactly once per daemon lifetime,
not once per document.

**Deliberately synchronous, not async.** Thread-per-connection over
`std::os::unix::net` rather than tokio/async-std: this project already
fought a real MSRV-pinning battle getting the `tokenizers` crate to compile
under this sandbox's rustc 1.75 (see the offline tokenizer section above);
adding an async runtime risked reopening that exact fight for a workload
that doesn't need epoll-scale concurrency — ingestion is a synchronous,
CPU-bound ~100-300ms computation per document, not a connection juggling
thousands of idle clients.

**Go orchestrator integration**: `streamRustCoreDaemon` in `main.go` dials
a *fresh* Unix socket connection per HTTP request (not a shared persistent
connection across concurrent requests) since the wire protocol is strictly
one-request-then-one-response per connection and sharing one across
concurrent goroutines would corrupt the framing. A fresh local socket
connect is sub-millisecond, so this costs essentially nothing while
avoiding a connection-pool's added failure modes. `/health` reports whether
the configured daemon is actually reachable; if it isn't (not configured,
crashed, or a transient error), `handleIngest` logs why and transparently
falls back to `streamRustCoreSubprocess` — the always-correct baseline —
rather than failing the request.

**Measured, not assumed:**

| scenario | per-request latency |
|---|---|
| subprocess spawn per request (baseline) | ~110-150ms, every request |
| daemon, first request (cold) | ~145-175ms |
| daemon, subsequent requests (warm) | **~4-12ms** |
| 6 concurrent requests through the daemon (3 documents × 2 fixtures) | all correct, no cross-request data mixing |

That's roughly a **12-30x** reduction in steady-state per-request latency,
concretely delivering the ticket's goal ("drops processing latencies down
to pure computation times") rather than just asserting it.

**Trade-off, stated plainly:** the daemon path buffers the entire NDJSON
response before sending any of it (one length-prefixed blob per request,
not a byte stream), so a caller loses the "see page 1 before page 500 is
parsed" incremental behavior the subprocess path provides. Given the
daemon's own per-request latency is single-digit-to-low-tens of
milliseconds once warm, that's a reasonable trade — but it's a real
behavioral difference, not just an implementation detail, and is why the
subprocess path remains the default (daemon mode is opt-in via
`RUST_DAEMON_SOCKET`) rather than a hard cutover.

## Track C: Enterprise Productization Layer (implemented)

Three features, built on top of Track A/B without touching the Rust core's
extraction logic (one small, well-justified exception noted below): a
multi-tenant quarantine review API, Prometheus-compatible metrics with
structured tracing, and native vector-database export connectors.

### 1. Multi-Tenant Audit & Quarantine API

`GET /api/v1/quarantine`, `GET /api/v1/quarantine/{id}`,
`POST /api/v1/quarantine/{id}/approve`, `.../reject` — all behind
`X-API-Key` tenant authentication (`auth.go`).

**One necessary Rust-core change, not scope creep:** quarantined pages
never reached the chunker before, so the NDJSON stream never carried their
actual text — only a fidelity *score*. "Inspect why the score dropped and
edit the Markdown" needs the Markdown. `main.rs`'s `page_fidelity` record
now includes a `"markdown"` field, but *only* for quarantined pages —
passed/warned pages are unchanged, since their text already flows through
`chunk` records. This is a 12-line, precisely-scoped addition, not a
reopening of the extraction pipeline.

**The tenant boundary is real, not cosmetic**, and it's proven, not just
implemented: `Store.Get`/`List`/`Review` all *require* a tenant ID and
filter by it (`store.go`) — there is no code path that returns another
tenant's data. Tested directly (`TestStoreTenantIsolation`) and over HTTP
(cross-tenant `GET`/`approve` attempts on a real record ID both return 404,
not data, not a permission-denied-but-confirms-existence response).

**Persistence is a file-backed JSON store**, deliberately — "light admin
API" was the ask, and this deploys with zero external infrastructure.
Explicitly not a production-scale choice (whole-file rewrite per write, no
cross-process transaction safety); `Store`'s method-only access surface
means swapping the backing implementation later doesn't touch any caller.

**Approved records flow into export** (ties into Feature 3): `POST
/api/v1/export` with `"include_approved_quarantine": true` pulls a
tenant's approved records (admin's edited text if provided, original
extraction otherwise — `PageRecord.FinalText()`) and exports them
alongside normal chunks through the same connector. Verified end-to-end:
ingest → quarantine → approve-with-edit → export → confirmed correct row
in a live Postgres.

### 2. Metrics & Tracing

**The real OpenTelemetry SDK and the real Prometheus client library are
both unreachable in this sandbox — verified directly, not assumed.**
`go get go.opentelemetry.io/otel` fails on the vanity import domain itself
(`go.opentelemetry.io` isn't in the network allowlist). The official
Prometheus client fetches fine from `github.com` but its transitive deps
(`google.golang.org/protobuf`, `golang.org/x/sys`) don't. Both failures are
reproducible by running the exact `go get` commands in `metrics.go`'s doc
comment.

**`/metrics`** (`metrics.go`) is a hand-rolled Prometheus text-exposition
endpoint — no client library, since the format itself is simple, stable,
and documented, and a real Prometheus server can't tell the difference.
**Validated against the actual Python `prometheus_client` parser**, not
eyeballed: `text_string_to_metric_families()` parses all 13 metric families
(12 counters + the `pdf_ingestion_latency_ms` histogram) without error.
Tracks `pages_quarantined_total`, `documents_ingested_total`,
`chunks_emitted_total`, `daemon_requests_total` vs
`subprocess_fallback_requests_total`, and more.

**One correction to the ticket's metric name:** `tokens_processed_total` is
exposed as a raw counter, not `tokens_processed_per_second`. A metric that
tries to *be* a rate breaks depending on scrape interval — Prometheus's
data model expects raw cumulative counters with rate computed at query
time (`rate(tokens_processed_total[1m])`). Naming it `_total` and
documenting the `rate()` query is the correct Prometheus pattern, not a
missing feature.

**Tracing** (`tracing.go`) is structured JSON span logs — `trace_id`,
`span_id`, `parent_span_id`, `duration_ms`, matching OTel's own data model
— rather than a hand-rolled OTLP/HTTP exporter. This was a deliberate
risk call: implementing OTLP's wire format by hand with no real collector
in this sandbox to validate against risks shipping something that *looks*
correct but silently isn't. Structured trace-ID-correlated logs are a
well-established, lower-risk alternative (Datadog explicitly supports
"logs as traces" via exactly this correlation), and because the span shape
already mirrors OTel's model, swapping in the real SDK later means
replacing `Span.End()`'s body, not redesigning the instrumentation
throughout the codebase.

### 3. Native Database Connectors

Qdrant and Pinecone connectors (`export_qdrant.go`, `export_pinecone.go`)
are plain REST calls via `net/http` — genuinely zero extra Go dependencies,
since both APIs are HTTP+JSON. pgvector (`export_pgvector.go`) needed one
dependency, `github.com/lib/pq` — the one Postgres driver this sandbox's
network egress could actually fetch (`jackc/pgx`, the more commonly
recommended modern driver, pulls in the same blocked `golang.org/x/*`
packages as the OTel/Prometheus libraries above).

**Tested against real, live services, not just unit-level assertions**:
- **pgvector**: a real Postgres 16 + pgvector extension instance, installed
  and running in this environment (`postgresql-16-pgvector` via apt).
  `TestPgVectorExporterIntegration` runs against it for real — INSERT,
  `ON CONFLICT DO UPDATE`, and a NULL-embedding row all verified by
  querying the actual table afterward, not by mocking `database/sql`.
- **Qdrant / Pinecone**: no self-hostable-in-this-sandbox option for
  either (Qdrant's a separate service; Pinecone is cloud-only), so these
  are tested against `httptest` servers standing in for their documented
  REST contracts — collection auto-creation, upsert payload shape, and
  auth headers are all asserted against the actual JSON bytes sent, not
  just "did the function return nil."

**The "zero extra glue code" claim needed one honest caveat, stated
directly rather than glossed over**: a vector database stores *vectors*.
This pipeline is deliberately zero-AI, deterministic extraction — nothing
in it generates embeddings, and that's a project goal, not a gap to patch
silently by hardcoding a paid embedding API and an assumed API key policy
nobody asked for. `Embedder` is a pluggable interface instead:
`NullEmbedder` (the default — metadata-only export, genuinely useful on
its own for keyword/full-text search) or `HTTPEmbedder`, which targets the
widely-adopted OpenAI-compatible `/v1/embeddings` request shape (works
with OpenAI itself, or self-hosted alternatives like LocalAI/vLLM/Ollama).
**Verified working end-to-end**, not just wired up: a mock OpenAI-shaped
embedding server → real vectors → real pgvector `VECTOR` column, confirmed
by querying Postgres afterward and seeing actual float arrays, not nulls.
Qdrant and Pinecone both require *some* vector on every point even for a
metadata-only export (there's no "vectors optional" mode on either API);
a 1-dimensional zero placeholder is used and documented as exactly what it
is — an inert placeholder, not a claim that similarity search means
anything until a real embedder is configured.

## Track D: Embedded Admin Dashboard & Storage Persistence (implemented)

A local, human-readable persistence layer plus a single-file, zero-dependency
web dashboard on top of it — no more `curl` and manual JSON copy-paste to
work with the pipeline.

### Storage layer (`docstore.go`)

Every completed `/ingest` writes to `data/tenants/{tenant_id}/documents/{document_id}/`:
`source.pdf`, `full_extracted.md`, `chunks.json`, `audit_report.json`,
`trace_spans.json` — exactly the five files specced. Three read endpoints
sit on top (`documents_api.go`), all behind the same `X-API-Key` tenant
auth as everything else: `GET /api/v1/documents`, `.../{id}/chunks`,
`.../{id}/markdown`.

**`full_extracted.md` is reconstructed, not a direct pass-through** — the
Rust core emits chunk records, not a single stitched document, so
`buildStitchedMarkdown` (`main.go`) walks the chunks in emission order and
re-inserts a Markdown heading at each depth of `parent_hierarchy` whenever
it changes from the previous chunk, so the file reads like the source
document's actual structure rather than a flat concatenation.

**A real bug, found by reading the actual output, not by inspection:**
the first version updated the "previous hierarchy" tracker after writing
*each heading level* instead of once per chunk — which made a chunk's own
`## System Requirements` compare against itself (just-updated by writing
`# Product X` one line earlier) and get silently treated as "unchanged,"
dropping every sub-heading below the first level from the stitched output
document-wide. Caught by actually reading `full_extracted.md` after a real
ingest, not by unit tests alone (though two now lock in the fix:
`TestBuildStitchedMarkdownWritesAllHeadingLevelsForFirstChunk` reproduces
the exact scenario). Same lesson as Track A/B's own bugs: generate the
real artifact and look at it, don't just trust that the code compiles and
the individual pieces seemed right in isolation.

**Path traversal was a real risk here, not a theoretical one** —
`document_id` arrives directly from client-controlled multipart form
data, and it becomes a directory name. `sanitizePathComponent` collapses
`..` and strips path separators before any filesystem operation touches
it; `TestSanitizePathComponentActuallyPreventsEscape` asserts the
resolved directory never leaves the store's own base directory even for
an explicit `../../../../etc`-style payload, not just that the *string*
looks sanitized.

### Dashboard (`dashboard.go`, `dashboard.html`)

One HTML file (inline `<style>` and `<script>`, no external requests),
embedded into the Go binary via `//go:embed` and served at `GET /` and
`GET /dashboard`. No Node, no bundler, no network access needed at build
time — verified the resulting binary has zero new build-time
dependencies, and separately verified the embedded JavaScript's syntax
with `node --check` (a verification tool only; nothing in the shipped
product requires Node to exist).

Four panels, matching the spec: **Upload & Monitor** (drag-and-drop,
reads the `/ingest` NDJSON response via `fetch()` + a streaming
`ReadableStream` reader — not `EventSource`, since that requires
`text/event-stream`, and this pipeline's response is `application/x-ndjson`;
a manual line-buffered reader over the same streaming body achieves the
same "watch it happen live" effect against the actual endpoint that
exists); **Documents** (list, select, view chunks with their
`parent_hierarchy` breadcrumb, tap-to-copy, download the stitched `.md`);
**Quarantine** (the existing review API, with an editable textarea and
Approve/Reject); **Export** (a form over `POST /api/v1/export`, field set
swapping per target, so nobody hand-writes the JSON body).

**Honest scope note on the API key field:** it's kept in `localStorage`
for convenience across page loads. That's a reasonable choice for a
same-origin, self-operated internal tool where the person entering the
key is the same person it belongs to — but it is not the pattern a
public-facing product would use (that wants an httpOnly session cookie
and a real login flow, which needs a login endpoint this "light admin"
scope doesn't include). Said directly rather than left for someone to
discover later.

**Decoupling, not just a claim:** the dashboard's JavaScript talks to
`/ingest`, `/api/v1/*`, and `/health` only — never anything in
`rust-core/`. A future change to layout detection, CMap parsing, or the
tokenizer changes what those JSON responses *contain*, not their shape,
so the UI keeps working unmodified — the same contract Track C's export
connectors already rely on.

## Known simplifications (the honest list)

- **The dashboard's API key storage is `localStorage`, not a session
  cookie** (`dashboard.html`) — reasonable for a same-origin, self-operated
  internal tool, not the pattern a public-facing product would use. See
  Track D above.
- **The document store is a plain directory tree, not indexed** —
  `ListDocuments` reads every `trace_spans.json` under a tenant on every
  call. Fine at the scale "light admin" implies; would want an index (or a
  real database, same upgrade path as `store.go`) at high document counts.
- **The admin API's auth is intentionally minimal** (`auth.go`): a static
  API-key-to-tenant map, no rotation, no scopes/roles, no rate limiting.
  Right-sized for "light admin API" as asked; not a substitute for a real
  auth system (OAuth2/OIDC, short-lived tokens) in an actual production
  multi-tenant deployment.
- **The quarantine store is a file, not a database** (`store.go`): correct
  and durable for the expected volume (flagged pages across a modest
  number of documents), but whole-file-rewrite-per-write and no
  cross-process transaction safety. `Store`'s method-only interface makes
  swapping this out later a contained change.
- **Tracing is structured logs, not real OTLP** (`tracing.go`), and
  `/metrics` is a hand-rolled Prometheus exporter, not the official client
  library — both because this sandbox's network egress can't reach
  `go.opentelemetry.io` or `golang.org/x/*` (verified directly, see Track
  C above), not because either was skipped for convenience.
- **Vector DB connectors don't generate embeddings** — `NullEmbedder` is
  the default; `HTTPEmbedder` needs a real OpenAI-compatible endpoint
  configured. This pipeline is deliberately zero-AI extraction; embedding
  generation was never going to be free, and pretending otherwise would be
  worse than saying so directly.
- **Qdrant/Pinecone connectors are tested against `httptest` mocks of
  their documented REST contracts, not live instances** — neither is
  self-hostable in this sandbox (Qdrant's a separate service; Pinecone is
  cloud-only). pgvector, by contrast, runs against a real local Postgres
  instance in the test suite. If you have real Qdrant/Pinecone
  credentials, the connectors haven't been checked against the live APIs
  beyond what their public documentation specifies.
- **Font widths are now real** (Track A Priority 1) for simple fonts with
  `/Widths` and for standard-14 fonts via the vendored fallback table. Not
  fully general: Type0/CID fonts are only handled for the common 2-byte
  Identity-H/V case (a genuine variable-width CMap would need the CMap
  itself parsed for codespace boundaries), and Type3 fonts (rare, define
  glyphs as tiny content-stream programs rather than standard outlines)
  aren't handled at all and fall back to the flat estimate.
- **Form XObject recursion is now implemented** (Track B Priority 1) —
  text and vector lines inside reusable form templates are extracted and
  fed into ruled-grid detection like any other content. Not fully general:
  BBox clipping is approximated (see the Track B section above), and CMap
  resolution inside a form still inherits the same 2-byte Identity-H/V
  assumption as the rest of the pipeline.
- **Ruled-table detection (Track A Priority 2) and the spatial-alignment
  heuristic are complements, not one replacing the other** — see the Track A
  section above for why "100% accurate" only holds for tables that actually
  have drawn borders, and for the real borderless-table-vs-columns bug this
  surfaced and how it was fixed.
- **Column detection (Track A Priority 3)** doesn't handle mixed
  full-width-banner + multi-column-body layouts on the same page, and its
  words-per-line table-vs-prose signal could, in principle, misjudge a
  genuine narrow list of short items — see the Track A section above.
- **Vector line detection approximates Bezier curves as straight lines to
  their endpoint** (curves are essentially never used for table rules, so
  this only affects shapes that wouldn't be classified as rules anyway),
  and doesn't apply PNG-predictor un-filtering (predictors are almost
  exclusively used for image/xref streams, not content streams).
- **Token counts are exact**, via a real, offline, embedded BPE tokenizer
  (see "Offline tokenizer" section below) — this was previously a
  `words × 1.3` heuristic; it no longer is.
- **LZWDecode content streams pass through undecoded.** Rare in modern PDF
  producers; ASCII85Decode + FlateDecode (what reportlab, and several other
  real-world producers, use) and plain FlateDecode are both fully supported
  via our own filter-chain decoder (`pdf_extract::decode_stream`) — this was
  necessary because lopdf 0.32's built-in decoder only understands a single
  Flate or LZW filter and silently falls back to raw encoded bytes on
  anything else, which was the root cause of zero text extraction the first
  time this was run against a real PDF.
- **Whole-file read for parsing.** PDF's xref table structure requires
  random/backward access, so the raw bytes are read into memory once before
  any processing starts — this is a format-level constraint, not a
  violation of the streaming architecture's intent, which applies to
  everything *after* that initial read (filtering, scoring, rendering,
  chunking all process one page group at a time).

## What to build next, roughly in priority order

1. CMap parsing for genuinely variable-width Type0/CID fonts, beyond the
   current 2-byte Identity-H/V assumption.
2. Mixed-layout support in column detection (full-width banner + multi-column
   body on the same page) — currently degrades to 1 column rather than
   detecting the boundary.
3. True geometric BBox clipping for Form XObjects (currently whole-element
   center-point filtering — see Track B Priority 1) for cases where a form's
   content genuinely straddles its clip boundary.
4. If concurrent load ever demands more than thread-per-connection can
   comfortably provide, a bounded worker-thread pool (or, if truly
   necessary, an async runtime) for the daemon — `daemon.rs`'s
   `handle_connection` is already decoupled from how connections are
   accepted, so this would be a contained change, not a rewrite.
5. A real connection pool for the Go orchestrator's daemon client, if
   per-request fresh-socket-dial overhead ever becomes measurable under
   very high request rates (measured as sub-millisecond in this project's
   testing, so not yet a real bottleneck).
6. A real auth system (OAuth2/OIDC) in front of the admin/export APIs,
   replacing the static API-key map — see Track C's "Known simplifications"
   entry.
7. Swap the quarantine store's file backend for Postgres/SQLite if
   deployment volume ever exceeds what a single-writer JSON file handles
   comfortably — `store.go`'s method-only interface was written so this is
   a contained change.
8. Real OTLP/HTTP export once this runs somewhere with network access to
   `go.opentelemetry.io` and a collector to validate against — `Span.End()`
   in `tracing.go` is the single point where that would plug in.
9. Verification against live Qdrant/Pinecone instances (currently tested
   against `httptest` mocks of their documented contracts — see Track C's
   "Known simplifications" entry).
10. A real login flow (session cookies, not a `localStorage` API key) if
    the dashboard is ever exposed beyond a trusted internal network — see
    Track D's "Known simplifications" entry.
