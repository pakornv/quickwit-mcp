#!/usr/bin/env bash
set -euo pipefail

# The test creates and removes only this disposable container.
engine="${CONTAINER_ENGINE:-docker}"
container="$("$engine" run -d --rm -p 127.0.0.1::7280 docker.io/quickwit/quickwit:0.8.2 run)"
trap '"$engine" rm -f "$container" >/dev/null 2>&1 || true' EXIT
address="$("$engine" port "$container" 7280/tcp | head -n 1)"
url="http://$address"

for attempt in {1..60}; do
  if curl -fsS "$url/api/v1/indexes" >/dev/null 2>&1; then
    break
  fi
  if [ "$attempt" -eq 60 ]; then
    "$engine" logs "$container"
    echo "Quickwit did not become ready" >&2
    exit 1
  fi
  sleep 1
done

curl -fsS -X POST "$url/api/v1/indexes" -H 'Content-Type: application/json' --data-binary @- >/dev/null <<'JSON'
{
  "version": "0.8",
  "index_id": "quickwit-mcp-test",
  "doc_mapping": {
    "timestamp_field": "timestamp",
    "field_mappings": [
      {"name": "timestamp", "type": "datetime", "input_formats": ["unix_timestamp"], "fast": true},
      {"name": "log", "type": "text"}
    ]
  },
  "search_settings": {"default_search_fields": ["log"]}
}
JSON

curl -fsS -X POST "$url/api/v1/quickwit-mcp-test/ingest?commit=force" -H 'Content-Type: application/x-ndjson' --data-binary @- >/dev/null <<'NDJSON'
{"timestamp":1700000000,"log":"first"}
{"timestamp":1700000060,"log":"second"}
{"timestamp":1700000120,"log":"third"}
NDJSON

QUICKWIT_TEST_URL="$url" go test -tags=integration -count=1 -run '^TestNewServer_Integration$' ./internal/quickwitmcp
