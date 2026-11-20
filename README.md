# quickwit-mcp

A small, read-only MCP server for investigating Quickwit logs. It connects an MCP client to a configured Quickwit REST origin over HTTP or HTTPS and communicates with the client over **stdio**.

The server is tested against **Quickwit 0.8.2**. Other versions may work, but have not been verified. It uses the [official Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk) and the [Quickwit 0.8.2 REST API](https://github.com/quickwit-oss/quickwit/blob/v0.8.2/docs/reference/rest-api.md).

## Install

Requires Go 1.27.4 or newer. From a local checkout:

```sh
go install ./cmd/quickwit-mcp
```

The binary is placed in `$(go env GOPATH)/bin` unless `GOBIN` is set. Once this repository has been published, install with:

```sh
go install github.com/pakornv/quickwit-mcp/cmd/quickwit-mcp@latest
```

Tagged releases can also provide prebuilt archives and checksums.

## Configure your MCP client

The only required setting is `QUICKWIT_URL`: an HTTP(S) **origin**, including the port if needed. Paths, query parameters, fragments, and URL credentials are rejected. Use an absolute binary path if the client does not inherit your shell's `PATH`.

Example `mcpServers` configuration:

```json
{
  "mcpServers": {
    "quickwit": {
      "command": "/absolute/path/to/quickwit-mcp",
      "args": [],
      "env": {
        "QUICKWIT_URL": "http://127.0.0.1:7280"
      }
    }
  }
}
```

For Pi, merge the entry into `~/.local/share/pi/agent/mcp.json` and run `/reload`. For other clients, follow their instructions for stdio MCP servers. Standard output is reserved for the protocol; startup errors go to stderr.

## Tools

| Tool | Purpose |
| --- | --- |
| `list_indexes(name_contains?, name_prefix?)` | Discover index IDs and timestamp fields; filter by a case-insensitive substring or a case-sensitive prefix. |
| `get_index_schema(index_id)` | Inspect one index's document mapping, tokenizers and default search fields. Does not return index storage/source settings. |
| `search(index_id, query, start_timestamp, end_timestamp, max_hits?, start_offset?, oldest_first?)` | Search one index using native Quickwit query syntax. |

`name_prefix` is a literal, case-sensitive index ID prefix applied by Quickwit before returning metadata. `name_contains` remains case-insensitive and is applied locally; both filters can be combined. If listing exceeds the response limit, use a narrower prefix, or call `get_index_schema` directly when the exact index ID is known.

Example `search` arguments for synthetic logs:

```json
{
  "index_id": "application-logs",
  "query": "log:\"ORDER_ID\"",
  "start_timestamp": 1700000000,
  "end_timestamp": 1700003600,
  "max_hits": 20,
  "oldest_first": true
}
```

Timestamps are UNIX **seconds**: the start is inclusive and the end is exclusive. Results are newest first unless `oldest_first` is true. Set `max_hits: 0` for a count-only query. For another page, keep the index, query, window and sort unchanged, and pass the returned `next_offset` as `start_offset` and `next_max_hits` as `max_hits`. The next page size may be smaller near the 10,000-hit limit.

Search result JSON is limited to 256 KiB per copy. MCP returns both text and structured copies, so the wire size is larger. `hits_truncated: true` means the output limit shortened this page; the next offset starts after the last hit actually returned. If one hit cannot fit, or search metadata alone exceeds the limit, the tool returns an error rather than skipping data.

Both `next_offset` and `next_max_hits` are null when there is no next page. This does not prove all matches were inspected. Offset pagination is not a snapshot: new ingestion or equal sort timestamps can change page contents. To search past the 10,000-hit ceiling or over multiple days, split the investigation into smaller time windows.

## Enforced limits

- Only index metadata GETs and search POSTs; no ingestion, delete, arbitrary URL, or admin tools.
- Tool arguments must use the documented names; unknown arguments are rejected.
- One exact index ID per search: no wildcard, multiple indexes, or path components.
- A positive search window of at most 24 hours, on an index with a timestamp field.
- Default 20 hits, maximum 100 per call; `start_offset + max_hits` cannot exceed 10,000. Backend responses exceeding the requested hit count are rejected.
- Search results are limited to 256 KiB of serialized JSON, including metadata and complete hits.
- Nonblank queries of at most 4,096 Unicode characters.
- A 15-second total timeout per HTTP request and a 4 MiB response limit; errors include at most 2,000 bytes of backend response.
- No HTTP redirects or environment proxies. HTTP and malformed-response failures are tool errors; Quickwit partial-result errors are returned.

These limits reduce accidental scope, but **do not enforce a backend compute budget**. Time filtering and sorting depend on the index's timestamp field; indexes without one cannot be searched.

## Security and privacy

This server does **not** provide upstream authentication, per-index authorization, TLS termination, or automatic secret/PII redaction. HTTPS uses the system trust configuration. HTTP sends queries and results in plaintext; use it only on a trusted network. Restrict network access and configure read-only authorization at the gateway/backend where available.

Matching log records, schema information, queries, and backend error details are returned to the MCP client and may be sent to its model provider. They can contain credentials or personal data. Log text is untrusted data and can contain prompt-injection attempts; server instructions are guidance, not a security boundary. Tool annotations are hints, not access control.

See [SECURITY.md](SECURITY.md) for vulnerability reporting.

## Development

```sh
go test ./...
go test -race ./...
go vet ./...
```

Tests use synthetic data, an in-process MCP client and local mock HTTP servers; a subprocess test exercises the actual stdio binary.

To additionally test against a disposable Quickwit 0.8.2 container, install Docker and curl, then run:

```sh
./scripts/test-quickwit.sh
# Or: CONTAINER_ENGINE=podman ./scripts/test-quickwit.sh
```

The script starts a new container bound to a random localhost port, creates a synthetic index, checks prefix filtering, time boundaries, sort order, pagination and count-only search, and removes the container on exit. It does not connect to the `QUICKWIT_URL` in your environment.

## License

[MIT](LICENSE).
