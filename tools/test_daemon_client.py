"""
Minimal test client for rust-core's persistent daemon mode (Track B
Priority 4). Connects to the daemon's Unix Domain Socket, sends one or more
PDFs sequentially over the SAME connection (proving connection reuse works
and demonstrating the cold-vs-warm latency difference), and prints each
response's summary record.

Usage:
    # In one terminal:
    cd rust-core && ./target/release/rag_ingestion_core --daemon /tmp/rag.sock

    # In another:
    python3 tools/test_daemon_client.py /tmp/rag.sock \\
      testdata/test_financial_report.pdf testdata/test_financial_report.pdf

The second (and any subsequent) request on the same connection should show
a dramatically lower round-trip time than the first, since the daemon's
embedded tokenizer and other one-time setup costs are already paid.
"""
import socket
import struct
import json
import sys
import time


def send_request(sock, document_id: str, pdf_path: str) -> str:
    with open(pdf_path, "rb") as f:
        pdf_bytes = f.read()
    doc_id_bytes = document_id.encode("utf-8")

    sock.sendall(struct.pack(">I", len(doc_id_bytes)) + doc_id_bytes)
    sock.sendall(struct.pack(">I", len(pdf_bytes)) + pdf_bytes)

    (resp_len,) = struct.unpack(">I", recv_exact(sock, 4))
    return recv_exact(sock, resp_len).decode("utf-8")


def recv_exact(sock, n: int) -> bytes:
    buf = b""
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise ConnectionError("socket closed early")
        buf += chunk
    return buf


def main():
    socket_path = sys.argv[1]
    fixtures = sys.argv[2:]

    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    sock.connect(socket_path)

    for i, fixture in enumerate(fixtures):
        start = time.time()
        response = send_request(sock, f"daemon_test_doc_{i}", fixture)
        elapsed_ms = (time.time() - start) * 1000
        lines = [json.loads(l) for l in response.splitlines() if l.strip()]
        summary = next((l for l in lines if l.get("type") == "summary"), None)
        print(f"[{fixture}] round-trip={elapsed_ms:.1f}ms lines={len(lines)} summary={summary}")

    sock.close()


if __name__ == "__main__":
    main()
