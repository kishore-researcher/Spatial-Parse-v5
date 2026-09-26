use crate::font_metrics::{self, FontMetrics};
use crate::types::{LineSegment, PageContent, TextElement};
use lopdf::content::{Content, Operation};
use lopdf::{Dictionary, Document, Object, ObjectId, Stream};
use std::collections::{HashMap, HashSet};
use std::error::Error;

/// Decodes an Adobe ASCII85-encoded byte stream (the `<~ ... ~>` wrapped
/// base-85 encoding used by many PDF producers, including reportlab, ahead
/// of FlateDecode). lopdf 0.32's built-in decoder does not support this
/// filter at all, which silently left every content stream from such
/// producers undecoded -- this was the root cause of zero text elements
/// being extracted from ASCII85-chain PDFs during testing.
fn ascii85_decode(input: &[u8]) -> Vec<u8> {
    let mut out = Vec::new();
    let mut group = [0u8; 5];
    let mut group_len = 0usize;
    let mut iter = input.iter().peekable();

    // Skip an optional leading "<~" delimiter.
    if input.starts_with(b"<~") {
        iter.next();
        iter.next();
    }

    while let Some(&b) = iter.next() {
        if b == b'~' {
            break; // trailing "~>" delimiter
        }
        if b.is_ascii_whitespace() {
            continue;
        }
        if b == b'z' && group_len == 0 {
            out.extend_from_slice(&[0, 0, 0, 0]);
            continue;
        }
        if !(b'!'..=b'u').contains(&b) {
            continue; // skip anything unexpected rather than aborting
        }
        group[group_len] = b - b'!';
        group_len += 1;
        if group_len == 5 {
            let mut value: u32 = 0;
            for g in group {
                value = value.wrapping_mul(85).wrapping_add(g as u32);
            }
            out.extend_from_slice(&value.to_be_bytes());
            group_len = 0;
        }
    }

    if group_len > 0 {
        // Pad the final partial group with 'u' (84) as per spec, then keep
        // only (group_len - 1) output bytes.
        for g in group.iter_mut().skip(group_len) {
            *g = 84;
        }
        let mut value: u32 = 0;
        for g in group {
            value = value.wrapping_mul(85).wrapping_add(g as u32);
        }
        let bytes = value.to_be_bytes();
        out.extend_from_slice(&bytes[..group_len - 1]);
    }
    out
}

fn asciihex_decode(input: &[u8]) -> Vec<u8> {
    let mut digits = Vec::new();
    for &b in input {
        if b == b'>' {
            break;
        }
        if b.is_ascii_hexdigit() {
            digits.push(b);
        }
    }
    if digits.len() % 2 == 1 {
        digits.push(b'0');
    }
    digits
        .chunks(2)
        .filter_map(|pair| {
            let s = std::str::from_utf8(pair).ok()?;
            u8::from_str_radix(s, 16).ok()
        })
        .collect()
}

fn flate_decode(input: &[u8]) -> Vec<u8> {
    use flate2::read::ZlibDecoder;
    use std::io::Read;
    let mut decoder = ZlibDecoder::new(input);
    let mut out = Vec::with_capacity(input.len() * 3);
    let _ = decoder.read_to_end(&mut out); // best-effort, matches lopdf's own tolerance of truncated streams
    out
}

/// Normalizes a stream's /Filter entry (Name or Array of Names) into an
/// ordered list, since PDF allows filter *chains* (e.g. ASCII85 wrapping
/// Flate) applied in the order listed.
fn filter_chain(dict: &Dictionary) -> Vec<String> {
    match dict.get(b"Filter") {
        Ok(obj) => {
            if let Ok(name) = obj.as_name_str() {
                vec![name.to_string()]
            } else if let Ok(arr) = obj.as_array() {
                arr.iter().filter_map(|o| o.as_name_str().ok().map(String::from)).collect()
            } else {
                vec![]
            }
        }
        Err(_) => vec![],
    }
}

/// Our own stream-content decoder, used instead of lopdf's `decompressed_content`
/// because that method only understands a single FlateDecode/LZWDecode filter
/// and errors out (silently falling back to raw, still-encoded bytes) on any
/// other filter -- including the ASCII85Decode-then-FlateDecode chain that
/// reportlab (and various enterprise PDF producers) uses by default.
fn decode_stream(stream: &Stream) -> Vec<u8> {
    let filters = filter_chain(&stream.dict);
    let mut data = stream.content.clone();
    for filter in filters {
        data = match filter.as_str() {
            "ASCII85Decode" => ascii85_decode(&data),
            "ASCIIHexDecode" => asciihex_decode(&data),
            "FlateDecode" => flate_decode(&data),
            "LZWDecode" => {
                // Rare in modern content streams; not implemented in this
                // v1. Falling through with the data as-is is safer than
                // aborting the whole page, and is flagged in README as a
                // known gap.
                data
            }
            _ => data,
        };
    }
    data
}

/// Fallback glyph width (as a fraction of font size) used only when a
/// font resource can't be resolved at all (e.g. a malformed content stream
/// referencing an undefined /Tf name) -- real per-glyph widths from
/// font_metrics::FontMetrics are used everywhere else. See
/// resources/README.md for where the real metrics come from.
const UNRESOLVED_FONT_FALLBACK_WIDTH_EM: f64 = 0.5;

/// Hard cap on Form XObject recursion depth. Defends against both
/// accidental deep nesting and deliberately malicious self-referencing
/// XObjects (a known PDF-parser DoS vector) -- combined with the
/// `visiting` cycle guard in `Interpreter`, which catches true cycles
/// directly rather than just truncating them after N levels.
const MAX_XOBJECT_DEPTH: usize = 12;

#[derive(Clone, Copy, Debug)]
struct Matrix {
    a: f64,
    b: f64,
    c: f64,
    d: f64,
    e: f64,
    f: f64,
}

impl Matrix {
    fn identity() -> Self {
        Matrix { a: 1.0, b: 0.0, c: 0.0, d: 1.0, e: 0.0, f: 0.0 }
    }

    fn translation(tx: f64, ty: f64) -> Self {
        Matrix { a: 1.0, b: 0.0, c: 0.0, d: 1.0, e: tx, f: ty }
    }

    /// Row-vector convention: result = self * other, matching PDF's
    /// `[x y 1] * M` point transform and `CTM' = M_cm * CTM` concatenation.
    fn mul(&self, o: &Matrix) -> Matrix {
        Matrix {
            a: self.a * o.a + self.b * o.c,
            b: self.a * o.b + self.b * o.d,
            c: self.c * o.a + self.d * o.c,
            d: self.c * o.b + self.d * o.d,
            e: self.e * o.a + self.f * o.c + o.e,
            f: self.e * o.b + self.f * o.d + o.f,
        }
    }

    fn transform_point(&self, x: f64, y: f64) -> (f64, f64) {
        (x * self.a + y * self.c + self.e, x * self.b + y * self.d + self.f)
    }
}

#[derive(Clone)]
struct TextState {
    tf_size: f64,
    tc: f64,      // char spacing
    tw: f64,      // word spacing
    tz: f64,      // horizontal scaling, percent (default 100)
    tl: f64,      // leading
    trise: f64,   // rise
    tm: Matrix,   // text matrix
    tlm: Matrix,  // text line matrix
    font_key: String, // current /Tf resource name, e.g. "F1" -- looked up in the current scope's font table
}

impl TextState {
    fn new() -> Self {
        TextState {
            tf_size: 12.0,
            tc: 0.0,
            tw: 0.0,
            tz: 100.0,
            tl: 0.0,
            trise: 0.0,
            tm: Matrix::identity(),
            tlm: Matrix::identity(),
            font_key: String::new(),
        }
    }
}

fn as_f64(obj: &Object) -> f64 {
    match obj {
        Object::Integer(i) => *i as f64,
        Object::Real(r) => *r as f64,
        _ => 0.0,
    }
}

/// Walk up the page tree /Parent chain to resolve an inherited attribute
/// (needed for MediaBox, which many multi-page PDFs only set once on the
/// root Pages node rather than repeating on every leaf page).
fn resolve_inherited_mediabox(doc: &Document, mut obj_id: ObjectId) -> Option<(f64, f64, f64, f64)> {
    let mut depth = 0;
    loop {
        depth += 1;
        if depth > 64 {
            return None; // guard against malformed cyclic trees
        }
        let dict = match doc.get_object(obj_id).and_then(|o| o.as_dict()) {
            Ok(d) => d.clone(),
            Err(_) => return None,
        };
        if let Ok(arr) = dict.get(b"MediaBox").and_then(|o| o.as_array()) {
            if arr.len() == 4 {
                return Some((as_f64(&arr[0]), as_f64(&arr[1]), as_f64(&arr[2]), as_f64(&arr[3])));
            }
        }
        match dict.get(b"Parent").and_then(|o| o.as_reference()) {
            Ok(parent_id) => obj_id = parent_id,
            Err(_) => return None,
        }
    }
}

/// Resolves a page's `/XObject` resources into resource-name -> ObjectId,
/// mirroring lopdf's own `get_page_fonts` merge order exactly (the page's
/// direct /Resources dict takes precedence; ancestor Pages-tree /Resources
/// entries are merged in afterward, only filling in names not already
/// present) since XObject resources are inherited the same way font
/// resources are.
fn resolve_page_xobjects(doc: &Document, page_id: ObjectId) -> HashMap<Vec<u8>, ObjectId> {
    let mut out = HashMap::new();
    let (resource_dict, resource_ids) = doc.get_page_resources(page_id);
    if let Some(resources) = resource_dict {
        merge_xobjects_from_resources(doc, resources, &mut out);
    }
    for resource_id in resource_ids {
        if let Ok(resources) = doc.get_dictionary(resource_id) {
            merge_xobjects_from_resources(doc, resources, &mut out);
        }
    }
    out
}

fn merge_xobjects_from_resources(doc: &Document, resources: &Dictionary, out: &mut HashMap<Vec<u8>, ObjectId>) {
    let Some(xobj_dict) = resources.get(b"XObject").ok().and_then(|o| font_metrics::resolve_dict(doc, o)) else {
        return;
    };
    for (name, obj) in xobj_dict.iter() {
        if let Object::Reference(id) = obj {
            out.entry(name.clone()).or_insert(*id);
        }
    }
}

/// Threshold (in device-space points) below which a *filled* (not stroked)
/// shape's bounding-box width or height is considered "thin enough to be a
/// drawn rule line" rather than a shaded background block. Many PDF
/// producers draw table rules as a filled 0.5-1pt tall rectangle instead of
/// a stroked line -- this catches that case while still ignoring, say, a
/// colored header-row background fill (which is wide AND tall).
const THIN_FILL_THRESHOLD_PT: f64 = 2.5;

/// Transforms each subpath's points through the current CTM and, if the
/// paint operator actually draws something visible (not "n"), decides
/// whether each subpath's edges qualify as rule-line candidates:
///   - Any stroked path's edges always count (a stroke *is* a drawn line).
///   - A filled (but not stroked) path's edges only count if the shape is
///     "thin" in one dimension -- see THIN_FILL_THRESHOLD_PT -- so a large
///     colored background rectangle isn't mistaken for a ruled border.
fn paint_path(subpaths: &[Vec<(f64, f64)>], ctm: &Matrix, is_stroke: bool, is_fill: bool, out: &mut Vec<LineSegment>) {
    for sp in subpaths {
        if sp.len() < 2 {
            continue;
        }
        let transformed: Vec<(f64, f64)> = sp.iter().map(|&(x, y)| ctm.transform_point(x, y)).collect();

        let counts_as_rule = if is_stroke {
            true
        } else if is_fill {
            let min_x = transformed.iter().map(|p| p.0).fold(f64::INFINITY, f64::min);
            let max_x = transformed.iter().map(|p| p.0).fold(f64::NEG_INFINITY, f64::max);
            let min_y = transformed.iter().map(|p| p.1).fold(f64::INFINITY, f64::min);
            let max_y = transformed.iter().map(|p| p.1).fold(f64::NEG_INFINITY, f64::max);
            (max_x - min_x) <= THIN_FILL_THRESHOLD_PT || (max_y - min_y) <= THIN_FILL_THRESHOLD_PT
        } else {
            false
        };

        if !counts_as_rule {
            continue;
        }
        for pair in transformed.windows(2) {
            out.push(LineSegment { x0: pair[0].0, y0: pair[0].1, x1: pair[1].0, y1: pair[1].1 });
        }
    }
}

fn emit_text_run(
    text: &str,
    ts: &TextState,
    ctm: &Matrix,
    page_num: usize,
    raw_bytes: &[u8],
    font_table: &HashMap<String, FontMetrics>,
) -> Option<TextElement> {
    let advance = estimate_advance(raw_bytes, ts, font_table);
    emit_text_run_at(text, ts, &ts.tm, ctm, advance, page_num)
}

/// Builds a TextElement's bounding box by transforming the text-space origin
/// (0,0) and the run's total advance through the text rendering matrix
/// (Trm = scale(Tfs*Th, Tfs) x Tm x CTM), using an explicit anchor matrix
/// `anchor_tm` rather than reading `ts.tm` directly -- needed for TJ, where
/// the text matrix has already moved past the run's start by the time we
/// know its full text and total advance.
fn emit_text_run_at(
    text: &str,
    ts: &TextState,
    anchor_tm: &Matrix,
    ctm: &Matrix,
    advance: f64,
    page_num: usize,
) -> Option<TextElement> {
    if text.trim().is_empty() {
        return None;
    }
    let trm = Matrix {
        a: ts.tf_size * (ts.tz / 100.0),
        b: 0.0,
        c: 0.0,
        d: ts.tf_size,
        e: 0.0,
        f: ts.trise,
    }
    .mul(anchor_tm)
    .mul(ctm);

    let (ox, oy) = trm.transform_point(0.0, 0.0);
    // advance is already in unscaled text-space units accumulated by the
    // caller; dividing by tf_size undoes the d-scaling baked into `trm` so
    // the transform_point below applies it back consistently.
    let (ex, ey) = trm.transform_point(advance / ts.tf_size.max(0.01), 0.0);
    let _ = ey;

    let (x0, x1) = if ox <= ex { (ox, ex) } else { (ex, ox) };
    // Approximate ascent/descent as fractions of font size in device space.
    // This ignores page rotation, which is a documented v1 limitation.
    let rough_height = ts.tf_size.abs().max(1.0);
    let y0 = oy - 0.25 * rough_height;
    let y1 = oy + 0.75 * rough_height;
    let (y0, y1) = if y0 <= y1 { (y0, y1) } else { (y1, y0) };

    Some(TextElement {
        text: text.to_string(),
        x_min: x0,
        y_min: y0,
        x_max: x1,
        y_max: y1,
        font_size: ts.tf_size,
        page_num,
    })
}

/// Text-space advance (before matrix transform), following the PDF advance
/// formula but sourcing each glyph's width (`w0` in the PDF spec's own
/// terms) from the current font's real metrics -- /Widths, /W (CID), or a
/// standard-14 core font table, resolved via `font_table` -- instead of a
/// flat heuristic. Operates on raw PDF string bytes rather than the
/// lossy-decoded display text, since widths are keyed by character *code*,
/// and because composite (Type0) fonts need to consume 2 bytes per glyph
/// rather than 1.
fn estimate_advance(bytes: &[u8], ts: &TextState, font_table: &HashMap<String, FontMetrics>) -> f64 {
    let metrics = font_table.get(&ts.font_key);
    let two_byte = metrics.map(|m| m.is_two_byte()).unwrap_or(false);

    let mut total = 0.0;
    if two_byte {
        for pair in bytes.chunks(2) {
            let code = if pair.len() == 2 { ((pair[0] as u32) << 8) | pair[1] as u32 } else { pair[0] as u32 };
            let w0 = metrics.map(|m| m.width_fraction(code)).unwrap_or(UNRESOLVED_FONT_FALLBACK_WIDTH_EM) * ts.tf_size;
            total += w0 + ts.tc;
            // Word spacing (Tw) only applies to single-byte code 32 per the
            // PDF spec -- never to any byte within a multi-byte code -- so
            // it's intentionally not added here for composite fonts.
        }
    } else {
        for &byte in bytes {
            let w0 = metrics.map(|m| m.width_fraction(byte as u32)).unwrap_or(UNRESOLVED_FONT_FALLBACK_WIDTH_EM) * ts.tf_size;
            total += w0 + ts.tc;
            if byte == b' ' {
                total += ts.tw;
            }
        }
    }
    total * (ts.tz / 100.0)
}

fn advance_by_bytes(ts: &mut TextState, bytes: &[u8], font_table: &HashMap<String, FontMetrics>) {
    let advance = estimate_advance(bytes, ts, font_table);
    ts.tm = Matrix::translation(advance, 0.0).mul(&ts.tm);
}

/// Recursive content-stream interpreter. One `Interpreter` is created per
/// page and shares its `elements`/`vector_lines`/`visiting` accumulators
/// across however many nested Form XObject calls `run` makes into itself
/// via `handle_do` -- so a Form XObject's text and vector lines land in the
/// exact same page-scoped output as everything else, already transformed
/// into top-level page device space (since the nested CTM is always seeded
/// from the parent CTM at the point of invocation).
struct Interpreter<'a> {
    doc: &'a Document,
    page_num: usize,
    elements: &'a mut Vec<TextElement>,
    vector_lines: &'a mut Vec<LineSegment>,
    visiting: &'a mut HashSet<ObjectId>,
}

impl<'a> Interpreter<'a> {
    fn run(
        &mut self,
        operations: &[Operation],
        ctm_init: Matrix,
        ts_init: TextState,
        font_table: &HashMap<String, FontMetrics>,
        xobject_table: &HashMap<Vec<u8>, ObjectId>,
        depth: usize,
    ) {
        let mut ctm = ctm_init;
        let mut ts = ts_init;
        let mut ctm_stack: Vec<Matrix> = Vec::new();
        let mut in_text_object = false;
        // Path construction state (m/l/re/c/v/y/h), consumed and cleared by
        // the next path-painting operator (S/s/f/F/f*/B/B*/b/b*/n), per the
        // PDF spec's path object lifecycle. Points are kept in raw
        // (untransformed) user-space coordinates and transformed through
        // the CTM only at paint time -- see doc comment on `paint_path`.
        let mut subpaths: Vec<Vec<(f64, f64)>> = Vec::new();

        for op in operations {
            match op.operator.as_str() {
                "q" => ctm_stack.push(ctm),
                "Q" => {
                    if let Some(m) = ctm_stack.pop() {
                        ctm = m;
                    }
                }
                "cm" => {
                    if op.operands.len() == 6 {
                        let m = Matrix {
                            a: as_f64(&op.operands[0]),
                            b: as_f64(&op.operands[1]),
                            c: as_f64(&op.operands[2]),
                            d: as_f64(&op.operands[3]),
                            e: as_f64(&op.operands[4]),
                            f: as_f64(&op.operands[5]),
                        };
                        ctm = m.mul(&ctm);
                    }
                }
                "BT" => {
                    in_text_object = true;
                    ts.tm = Matrix::identity();
                    ts.tlm = Matrix::identity();
                }
                "ET" => {
                    in_text_object = false;
                }
                "m" => {
                    if op.operands.len() == 2 {
                        subpaths.push(vec![(as_f64(&op.operands[0]), as_f64(&op.operands[1]))]);
                    }
                }
                "l" => {
                    if op.operands.len() == 2 {
                        let pt = (as_f64(&op.operands[0]), as_f64(&op.operands[1]));
                        match subpaths.last_mut() {
                            Some(sp) => sp.push(pt),
                            None => subpaths.push(vec![pt]),
                        }
                    }
                }
                "c" | "v" | "y" => {
                    // Bezier curves: for rule-line detection purposes we
                    // only need the endpoint, not the true curve -- table
                    // rules are essentially never drawn as curves, so
                    // approximating the curve as a straight line to its
                    // endpoint is a safe simplification (it only affects
                    // shapes we wouldn't classify as rules anyway).
                    if op.operands.len() >= 2 {
                        let n = op.operands.len();
                        let x = as_f64(&op.operands[n - 2]);
                        let y = as_f64(&op.operands[n - 1]);
                        match subpaths.last_mut() {
                            Some(sp) => sp.push((x, y)),
                            None => subpaths.push(vec![(x, y)]),
                        }
                    }
                }
                "h" => {
                    // Closepath: connect back to the subpath's own start.
                    if let Some(sp) = subpaths.last_mut() {
                        if let Some(&start) = sp.first() {
                            sp.push(start);
                        }
                    }
                }
                "re" => {
                    if op.operands.len() == 4 {
                        let x = as_f64(&op.operands[0]);
                        let y = as_f64(&op.operands[1]);
                        let w = as_f64(&op.operands[2]);
                        let h = as_f64(&op.operands[3]);
                        subpaths.push(vec![(x, y), (x + w, y), (x + w, y + h), (x, y + h), (x, y)]);
                    }
                }
                "S" | "s" | "f" | "F" | "f*" | "B" | "B*" | "b" | "b*" | "n" => {
                    let is_stroke = matches!(op.operator.as_str(), "S" | "s" | "B" | "B*" | "b" | "b*");
                    let is_fill = matches!(op.operator.as_str(), "f" | "F" | "f*" | "B" | "B*" | "b" | "b*");
                    if op.operator.as_str() != "n" {
                        paint_path(&subpaths, &ctm, is_stroke, is_fill, self.vector_lines);
                    }
                    subpaths.clear(); // any paint op (including 'n') consumes and clears the current path
                }
                "Do" => {
                    if let Some(Object::Name(name)) = op.operands.first() {
                        self.handle_do(name, ctm, &ts, font_table, xobject_table, depth);
                    }
                }
                "Tf" => {
                    if op.operands.len() == 2 {
                        ts.tf_size = as_f64(&op.operands[1]);
                        if let Object::Name(name) = &op.operands[0] {
                            ts.font_key = String::from_utf8_lossy(name).to_string();
                        }
                    }
                }
                "Tc" => {
                    if let Some(v) = op.operands.first() {
                        ts.tc = as_f64(v);
                    }
                }
                "Tw" => {
                    if let Some(v) = op.operands.first() {
                        ts.tw = as_f64(v);
                    }
                }
                "Tz" => {
                    if let Some(v) = op.operands.first() {
                        ts.tz = as_f64(v);
                    }
                }
                "TL" => {
                    if let Some(v) = op.operands.first() {
                        ts.tl = as_f64(v);
                    }
                }
                "Ts" => {
                    if let Some(v) = op.operands.first() {
                        ts.trise = as_f64(v);
                    }
                }
                "Td" => {
                    if op.operands.len() == 2 {
                        let tx = as_f64(&op.operands[0]);
                        let ty = as_f64(&op.operands[1]);
                        ts.tlm = Matrix::translation(tx, ty).mul(&ts.tlm);
                        ts.tm = ts.tlm;
                    }
                }
                "TD" => {
                    if op.operands.len() == 2 {
                        let tx = as_f64(&op.operands[0]);
                        let ty = as_f64(&op.operands[1]);
                        ts.tl = -ty;
                        ts.tlm = Matrix::translation(tx, ty).mul(&ts.tlm);
                        ts.tm = ts.tlm;
                    }
                }
                "Tm" => {
                    if op.operands.len() == 6 {
                        let m = Matrix {
                            a: as_f64(&op.operands[0]),
                            b: as_f64(&op.operands[1]),
                            c: as_f64(&op.operands[2]),
                            d: as_f64(&op.operands[3]),
                            e: as_f64(&op.operands[4]),
                            f: as_f64(&op.operands[5]),
                        };
                        ts.tlm = m;
                        ts.tm = m;
                    }
                }
                "T*" => {
                    ts.tlm = Matrix::translation(0.0, -ts.tl).mul(&ts.tlm);
                    ts.tm = ts.tlm;
                }
                "Tj" => {
                    if let Some(Object::String(s, _)) = op.operands.first() {
                        let text = String::from_utf8_lossy(s).to_string();
                        if let Some(el) = emit_text_run(&text, &ts, &ctm, self.page_num, s, font_table) {
                            self.elements.push(el);
                        }
                        advance_by_bytes(&mut ts, s, font_table);
                    }
                }
                "'" => {
                    ts.tlm = Matrix::translation(0.0, -ts.tl).mul(&ts.tlm);
                    ts.tm = ts.tlm;
                    if let Some(Object::String(s, _)) = op.operands.first() {
                        let text = String::from_utf8_lossy(s).to_string();
                        if let Some(el) = emit_text_run(&text, &ts, &ctm, self.page_num, s, font_table) {
                            self.elements.push(el);
                        }
                        advance_by_bytes(&mut ts, s, font_table);
                    }
                }
                "\"" => {
                    if op.operands.len() == 3 {
                        ts.tw = as_f64(&op.operands[0]);
                        ts.tc = as_f64(&op.operands[1]);
                    }
                    ts.tlm = Matrix::translation(0.0, -ts.tl).mul(&ts.tlm);
                    ts.tm = ts.tlm;
                    if let Some(Object::String(s, _)) = op.operands.get(2) {
                        let text = String::from_utf8_lossy(s).to_string();
                        if let Some(el) = emit_text_run(&text, &ts, &ctm, self.page_num, s, font_table) {
                            self.elements.push(el);
                        }
                        advance_by_bytes(&mut ts, s, font_table);
                    }
                }
                "TJ" => {
                    if let Some(Object::Array(arr)) = op.operands.first() {
                        // Snapshot the anchor before this operator moves it,
                        // so the emitted element's origin is where the run
                        // visually starts, not where it ends.
                        let start_tm = ts.tm;
                        let mut combined = String::new();
                        let mut total_advance = 0.0_f64;
                        for item in arr {
                            match item {
                                Object::String(s, _) => {
                                    let frag = String::from_utf8_lossy(s).to_string();
                                    let frag_adv = estimate_advance(s, &ts, font_table);
                                    total_advance += frag_adv;
                                    combined.push_str(&frag);
                                    ts.tm = Matrix::translation(frag_adv, 0.0).mul(&ts.tm);
                                }
                                Object::Integer(_) | Object::Real(_) => {
                                    let adj = as_f64(item);
                                    let tx = (-adj / 1000.0) * ts.tf_size * (ts.tz / 100.0);
                                    total_advance += tx;
                                    ts.tm = Matrix::translation(tx, 0.0).mul(&ts.tm);
                                }
                                _ => {}
                            }
                        }
                        if let Some(el) = emit_text_run_at(&combined, &ts, &start_tm, &ctm, total_advance, self.page_num) {
                            self.elements.push(el);
                        }
                    }
                }
                _ => {}
            }
            let _ = in_text_object;
        }
    }

    /// Handles a `Do` operator referencing a Form XObject: recursively
    /// interprets its content stream as if it were textually substituted
    /// at the `Do` call site, wrapped in an implicit `q ... Q` (per PDF
    /// spec 8.10.2) -- which is exactly what passing *copies* of `ctm` and
    /// `ts` into the recursive call (rather than mutable references)
    /// gives us for free: whatever the form does to its own local CTM/text
    /// state can never leak back out to the caller's `run` frame.
    ///
    /// Image XObjects (the other legal `Do` target) are recognized via
    /// `/Subtype` and skipped -- they're raster data with no text or
    /// vector paths to extract.
    fn handle_do(
        &mut self,
        name: &[u8],
        ctm: Matrix,
        ts: &TextState,
        font_table: &HashMap<String, FontMetrics>,
        xobject_table: &HashMap<Vec<u8>, ObjectId>,
        depth: usize,
    ) {
        if depth >= MAX_XOBJECT_DEPTH {
            return;
        }
        let Some(&xobj_id) = xobject_table.get(name) else {
            return; // undefined resource name -- nothing to recurse into
        };
        if self.visiting.contains(&xobj_id) {
            return; // true cycle (e.g. a form invoking itself) -- stop, don't recurse forever
        }
        let doc = self.doc; // `&'a Document` is Copy; this borrow is independent of `&mut self`
        let Ok(Object::Stream(stream)) = doc.get_object(xobj_id) else {
            return;
        };
        let subtype = stream.dict.get(b"Subtype").ok().and_then(|o| o.as_name_str().ok()).unwrap_or("");
        if subtype != "Form" {
            return; // Image XObject or unrecognized -- no text/vectors inside
        }

        let form_matrix = match stream.dict.get(b"Matrix").ok().and_then(|o| o.as_array().ok()) {
            Some(arr) if arr.len() == 6 => Matrix {
                a: as_f64(&arr[0]),
                b: as_f64(&arr[1]),
                c: as_f64(&arr[2]),
                d: as_f64(&arr[3]),
                e: as_f64(&arr[4]),
                f: as_f64(&arr[5]),
            },
            _ => Matrix::identity(),
        };
        let nested_ctm = form_matrix.mul(&ctm);

        // Resources: the form's own /Resources if present, otherwise
        // inherited from the caller (per PDF spec 7.8.3) -- NOT merged,
        // matching how every mainstream PDF renderer treats this.
        let (nested_font_table, nested_xobject_table);
        let (font_table_ref, xobject_table_ref): (&HashMap<String, FontMetrics>, &HashMap<Vec<u8>, ObjectId>) =
            match stream.dict.get(b"Resources").ok().and_then(|o| font_metrics::resolve_dict(doc, o)) {
                Some(resources) => {
                    nested_font_table = font_metrics::resolve_fonts_from_resources_dict(doc, resources);
                    nested_xobject_table = {
                        let mut m = HashMap::new();
                        merge_xobjects_from_resources(doc, resources, &mut m);
                        m
                    };
                    (&nested_font_table, &nested_xobject_table)
                }
                None => (font_table, xobject_table),
            };

        // Optional BBox clip: PDF spec says the form's content is clipped
        // to its /BBox (in form space), transformed by the form's own
        // /Matrix and the CTM at invocation time. We approximate this by
        // filtering *whole elements/segments* to those whose center falls
        // within the transformed bbox, rather than true geometric clipping
        // (e.g. partially clipping a text run) -- adequate for keeping a
        // reused template's off-canvas content out of the page, which is
        // the failure mode that matters for RAG ingestion.
        let bbox_device = stream.dict.get(b"BBox").ok().and_then(|o| o.as_array().ok()).and_then(|arr| {
            if arr.len() == 4 {
                let (x0, y0, x1, y1) = (as_f64(&arr[0]), as_f64(&arr[1]), as_f64(&arr[2]), as_f64(&arr[3]));
                let corners = [(x0, y0), (x1, y0), (x1, y1), (x0, y1)];
                let transformed: Vec<(f64, f64)> = corners.iter().map(|&(x, y)| nested_ctm.transform_point(x, y)).collect();
                let min_x = transformed.iter().map(|p| p.0).fold(f64::INFINITY, f64::min);
                let max_x = transformed.iter().map(|p| p.0).fold(f64::NEG_INFINITY, f64::max);
                let min_y = transformed.iter().map(|p| p.1).fold(f64::INFINITY, f64::min);
                let max_y = transformed.iter().map(|p| p.1).fold(f64::NEG_INFINITY, f64::max);
                Some((min_x, min_y, max_x, max_y))
            } else {
                None
            }
        });

        let content_bytes = decode_stream(stream);
        let content = Content::decode(&content_bytes).unwrap_or(Content { operations: vec![] });

        let elements_before = self.elements.len();
        let lines_before = self.vector_lines.len();

        self.visiting.insert(xobj_id);
        self.run(&content.operations, nested_ctm, ts.clone(), font_table_ref, xobject_table_ref, depth + 1);
        self.visiting.remove(&xobj_id);

        if let Some((bx0, by0, bx1, by1)) = bbox_device {
            let tol = 1.0;
            let mut idx = 0usize;
            self.elements.retain(|e| {
                idx += 1;
                if idx <= elements_before {
                    return true;
                }
                let cx = (e.x_min + e.x_max) / 2.0;
                let cy = (e.y_min + e.y_max) / 2.0;
                cx >= bx0 - tol && cx <= bx1 + tol && cy >= by0 - tol && cy <= by1 + tol
            });
            let mut idx2 = 0usize;
            self.vector_lines.retain(|l| {
                idx2 += 1;
                if idx2 <= lines_before {
                    return true;
                }
                let cx = (l.x0 + l.x1) / 2.0;
                let cy = (l.y0 + l.y1) / 2.0;
                cx >= bx0 - tol && cx <= bx1 + tol && cy >= by0 - tol && cy <= by1 + tol
            });
        }
    }
}

/// Extracts one PageContent per page via a forward pass over each page's
/// content stream, recursing into Form XObjects (Track A Priority 1) as
/// encountered so text and vector lines hidden inside reusable structural
/// assets (common in templated invoices) are no longer silently skipped.
pub fn extract_document(bytes: &[u8]) -> Result<Vec<PageContent>, Box<dyn Error>> {
    let doc = Document::load_mem(bytes)?;
    let mut pages_out = Vec::new();

    // get_pages() returns a BTreeMap<page_number, ObjectId>, already sorted,
    // which is what lets us do "one page group at a time" processing.
    for (page_num, page_id) in doc.get_pages() {
        let (x0, y0, x1, y1) = resolve_inherited_mediabox(&doc, page_id)
            .unwrap_or((0.0, 0.0, 612.0, 792.0)); // US Letter fallback
        let page_width = (x1 - x0).abs();
        let page_height = (y1 - y0).abs();

        let mut content_bytes = Vec::new();
        for stream_id in doc.get_page_contents(page_id) {
            if let Ok(Object::Stream(stream)) = doc.get_object(stream_id) {
                content_bytes.extend_from_slice(&decode_stream(stream));
            }
        }
        let content = Content::decode(&content_bytes).unwrap_or(Content { operations: vec![] });
        let font_table = font_metrics::resolve_page_fonts(&doc, page_id);
        let xobject_table = resolve_page_xobjects(&doc, page_id);

        let mut elements = Vec::new();
        let mut vector_lines: Vec<LineSegment> = Vec::new();
        let mut visiting: HashSet<ObjectId> = HashSet::new();

        let mut interpreter = Interpreter {
            doc: &doc,
            page_num: page_num as usize,
            elements: &mut elements,
            vector_lines: &mut vector_lines,
            visiting: &mut visiting,
        };
        interpreter.run(&content.operations, Matrix::identity(), TextState::new(), &font_table, &xobject_table, 0);

        pages_out.push(PageContent {
            page_num: page_num as usize,
            page_height,
            page_width,
            elements,
            vector_lines,
        });
    }

    Ok(pages_out)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Reads a testdata fixture relative to the crate root (rust-core/),
    /// which is `cargo test`'s default working directory.
    fn read_fixture(name: &str) -> Vec<u8> {
        std::fs::read(format!("../testdata/{name}"))
            .unwrap_or_else(|e| panic!("failed to read testdata/{name}: {e} -- run testdata/generate_xobject_fixtures.py first"))
    }

    fn all_text(pages: &[PageContent]) -> String {
        pages.iter().flat_map(|p| p.elements.iter()).map(|e| e.text.as_str()).collect::<Vec<_>>().join(" ")
    }

    #[test]
    fn form_xobject_content_is_extracted_not_skipped() {
        // Without recursion, only "Invoice INV-2026-0088" (the page's own
        // direct content) would be extracted -- the entire subtotal/tax/
        // total block lives inside a Form XObject invoked via `Do`.
        let bytes = read_fixture("xobject_single_form.pdf");
        let pages = extract_document(&bytes).expect("extraction failed");
        let text = all_text(&pages);
        assert!(text.contains("Invoice INV-2026-0088"));
        assert!(text.contains("Subtotal: 4,200.00"), "Form XObject content missing: {text:?}");
        assert!(text.contains("Tax (8.25%): 346.50"), "Form XObject content missing: {text:?}");
        assert!(text.contains("Total Due: 4,546.50"), "Form XObject content missing: {text:?}");
    }

    #[test]
    fn nested_form_xobjects_recurse_to_depth_2() {
        // "outerform" invokes "innerform" via its own `Do` operator --
        // this only reaches "Line item: Widget A/B" if handle_do's
        // recursive call into `run` correctly threads through a second
        // level of Form XObject invocation, not just one.
        let bytes = read_fixture("xobject_nested_form_and_image.pdf");
        let pages = extract_document(&bytes).expect("extraction failed");
        let text = all_text(&pages);
        assert!(text.contains("Purchase Order PO-4471"));
        assert!(text.contains("Line Items (nested template)"));
        assert!(text.contains("Widget A x 12 @ 4.50 = 54.00"), "nested (depth-2) Form XObject content missing: {text:?}");
        assert!(text.contains("Widget B x 3 @ 19.99 = 59.97"), "nested (depth-2) Form XObject content missing: {text:?}");
    }

    #[test]
    fn image_xobject_is_skipped_without_crashing() {
        // The same fixture also draws a real Image XObject via `Do`.
        // Extraction must complete successfully and must not emit any
        // garbage "text" from attempting to parse raster bytes as
        // content-stream operators.
        let bytes = read_fixture("xobject_nested_form_and_image.pdf");
        let pages = extract_document(&bytes).expect("extraction must not fail on an Image XObject");
        // Every extracted element's text should be one of our known
        // strings -- nothing unexpected (e.g. binary garbage) leaked in.
        let known = ["Purchase Order PO-4471", "Line Items (nested template)", "Widget A", "Widget B"];
        for page in &pages {
            for el in &page.elements {
                assert!(
                    known.iter().any(|k| el.text.contains(k)),
                    "unexpected element text (possible image-data leak): {:?}",
                    el.text
                );
            }
        }
    }
}
