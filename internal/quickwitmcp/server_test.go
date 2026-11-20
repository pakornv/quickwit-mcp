package quickwitmcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pakornv/quickwit-mcp/internal/quickwitmcp"
)

const indexMetadata = `{"index_config":{"index_id":"logs-test","index_uri":"s3://private-storage","doc_mapping":{"timestamp_field":"timestamp","field_mappings":[{"name":"log","type":"text","tokenizer":"default"}]},"search_settings":{"default_search_fields":[]}},"sources":[{"credentials":"private-secret"}]}`
const searchResult = `{"num_hits":3,"hits":[{"log":"first","order_id":9007199254740993}],"errors":[{"message":"one split failed"}]}`

type recordedRequest struct {
	method string
	path   string
	body   map[string]any
}

type fakeQuickwit struct {
	index    string
	result   string
	status   int
	mu       sync.Mutex
	requests []recordedRequest
}

func (f *fakeQuickwit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{r.Method, r.URL.Path, body})
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/v1/indexes":
		fmt.Fprintf(w, "[%s]", f.index)
	case "/api/v1/indexes/logs-test":
		fmt.Fprint(w, f.index)
	case "/api/v1/logs-test/search":
		if f.status == http.StatusTemporaryRedirect {
			w.Header().Set("Location", "/must-not-be-followed")
		}
		if f.status != 0 {
			w.WriteHeader(f.status)
		}
		fmt.Fprint(w, f.result)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeQuickwit) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

func TestNewServer(t *testing.T) {
	t.Run("valid origins", func(t *testing.T) {
		t.Parallel()
		for _, origin := range []string{"http://localhost:7280", "http://localhost:7280/", "https://example.com", "http://[::1]:7280"} {
			if _, err := quickwitmcp.NewServer(origin); err != nil {
				t.Errorf("NewServer(%q): %v", origin, err)
			}
		}
	})

	t.Run("invalid origins", func(t *testing.T) {
		t.Parallel()
		for _, origin := range []string{"", "localhost:7280", "ftp://host", "http://", "http://user:password@host", "http://host/path", "http://host?x=1", "http://host?", "http://host#fragment", "http://host#"} {
			if _, err := quickwitmcp.NewServer(origin); err == nil {
				t.Errorf("NewServer(%q) should fail", origin)
			}
		}
	})

	t.Run("exposes three annotated tools", func(t *testing.T) {
		t.Parallel()
		session := newSession(t, &fakeQuickwit{index: indexMetadata})
		result, err := session.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tool := range result.Tools {
			names = append(names, tool.Name)
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint ||
				tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint ||
				tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
				t.Errorf("unsafe annotations on %s: %+v", tool.Name, tool.Annotations)
			}
		}
		if !reflect.DeepEqual(names, []string{"get_index_schema", "list_indexes", "search"}) {
			t.Errorf("tools = %v", names)
		}
	})

	t.Run("discovers indexes and hides storage and sources", func(t *testing.T) {
		t.Parallel()
		fake := &fakeQuickwit{index: indexMetadata}
		session := newSession(t, fake)
		indexes := callSuccess(t, session, "list_indexes", map[string]any{"name_contains": "TEST"})
		if !strings.Contains(indexes, `"index_id":"logs-test"`) || !strings.Contains(indexes, `"timestamp_field":"timestamp"`) {
			t.Errorf("indexes = %s", indexes)
		}
		if missing := callSuccess(t, session, "list_indexes", map[string]any{"name_contains": "missing"}); missing != `{"indexes":[]}` {
			t.Errorf("filtered indexes = %s", missing)
		}
		schema := callSuccess(t, session, "get_index_schema", map[string]any{"index_id": "logs-test"})
		if !strings.Contains(schema, `"field_mappings"`) || !strings.Contains(schema, `"default_search_fields":[]`) {
			t.Errorf("schema = %s", schema)
		}
		if strings.Contains(indexes+schema, "private-storage") || strings.Contains(indexes+schema, "private-secret") {
			t.Errorf("sensitive index settings leaked: %s %s", indexes, schema)
		}
	})

	t.Run("filters large index catalogs by a case-sensitive prefix", func(t *testing.T) {
		t.Parallel()
		patterns := make(chan string, 2)
		session := newSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			pattern := r.URL.Query().Get("index_id_patterns")
			patterns <- pattern
			if pattern == "" {
				fmt.Fprintf(w, "[%s%s]", strings.Repeat(" ", 4*1024*1024), indexMetadata)
				return
			}
			other := strings.Replace(indexMetadata, "logs-test", "Logs-TEST", 1)
			fmt.Fprintf(w, "[%s,%s]", indexMetadata, other)
		}))
		callError(t, session, "list_indexes", map[string]any{"name_contains": "TEST"}, "response exceeds 4 MiB")
		result := callSuccess(t, session, "list_indexes", map[string]any{"name_prefix": "logs-", "name_contains": "TEST"})
		if !strings.Contains(result, `"index_id":"logs-test"`) || strings.Contains(result, "Logs-TEST") {
			t.Errorf("filtered indexes = %s", result)
		}
		if first, second := <-patterns, <-patterns; first != "" || second != "logs-*" {
			t.Errorf("upstream patterns = %q, %q", first, second)
		}
	})

	t.Run("accepts a full-length index prefix", func(t *testing.T) {
		t.Parallel()
		prefix := "a" + strings.Repeat("b", 254)
		patterns := make(chan string, 1)
		session := newSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			patterns <- r.URL.Query().Get("index_id_patterns")
			fmt.Fprintf(w, "[%s]", strings.Replace(indexMetadata, "logs-test", prefix, 1))
		}))
		result := callSuccess(t, session, "list_indexes", map[string]any{"name_prefix": prefix})
		if !strings.Contains(result, prefix) || <-patterns != prefix {
			t.Errorf("full-length prefix failed: %s", result)
		}
	})

	t.Run("rejects invalid index prefixes before HTTP", func(t *testing.T) {
		t.Parallel()
		for _, prefix := range []string{"../logs", "logs-*", "logs,other", strings.Repeat("a", 256)} {
			t.Run(prefix, func(t *testing.T) {
				t.Parallel()
				fake := &fakeQuickwit{index: indexMetadata}
				session := newSession(t, fake)
				callError(t, session, "list_indexes", map[string]any{"name_prefix": prefix}, "")
				if reqs := fake.recorded(); len(reqs) != 0 {
					t.Errorf("unexpected HTTP requests: %+v", reqs)
				}
			})
		}
	})

	t.Run("searches and preserves partial errors and large numbers", func(t *testing.T) {
		t.Parallel()
		fake := &fakeQuickwit{index: indexMetadata, result: searchResult}
		session := newSession(t, fake)
		args := validSearch()
		args["max_hits"] = 1
		args["start_offset"] = 1
		result := callSuccess(t, session, "search", args)
		for _, fragment := range []string{`"next_offset":2`, `"next_max_hits":1`, `"hits_truncated":false`, `"order_id":9007199254740993`, `"one split failed"`} {
			if !strings.Contains(result, fragment) {
				t.Errorf("search result %s missing %s", result, fragment)
			}
		}
		reqs := fake.recorded()
		if len(reqs) != 2 {
			t.Fatalf("requests = %+v", reqs)
		}
		wantBody := map[string]any{"query": `log:"ORDER_ID"`, "start_timestamp": float64(100), "end_timestamp": float64(200), "max_hits": float64(1), "start_offset": float64(1), "sort_by": "timestamp"}
		if reqs[0].method != "GET" || reqs[0].path != "/api/v1/indexes/logs-test" || reqs[1].method != "POST" || reqs[1].path != "/api/v1/logs-test/search" || !reflect.DeepEqual(reqs[1].body, wantBody) {
			t.Errorf("requests = %+v", reqs)
		}
	})

	t.Run("rejects more hits than requested", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name   string
			max    int
			result string
		}{
			{"count only", 0, `{"num_hits":1,"hits":[{"log":"sensitive-record"}]}`},
			{"limited page", 1, `{"num_hits":2,"hits":[{"log":"sensitive-record"},{}]}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				session := newSession(t, &fakeQuickwit{index: indexMetadata, result: tc.result})
				args := validSearch()
				args["max_hits"] = tc.max
				result := call(t, session, "search", args)
				text := resultText(t, result)
				if !result.IsError || !strings.Contains(text, "more hits than requested") || strings.Contains(text, "sensitive-record") {
					t.Errorf("unexpected response: error=%v text=%s", result.IsError, text)
				}
			})
		}
	})

	t.Run("limits search output and paginates without skipping hits", func(t *testing.T) {
		t.Parallel()
		message := strings.Repeat("<", 12000) + strings.Repeat("x", 20*1024)
		session := newSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				fmt.Fprint(w, indexMetadata)
				return
			}
			var req struct {
				MaxHits     int `json:"max_hits"`
				StartOffset int `json:"start_offset"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			hits := make([]map[string]any, 0)
			for id := req.StartOffset; id < min(req.StartOffset+req.MaxHits, 4); id++ {
				hits = append(hits, map[string]any{"id": id, "log": message, "order_id": int64(9007199254740993)})
			}
			enc := json.NewEncoder(w)
			enc.SetEscapeHTML(false)
			_ = enc.Encode(map[string]any{"num_hits": 4, "hits": hits, "errors": []string{"one split failed"}})
		}))
		args := validSearch()
		args["max_hits"] = 3
		var page struct {
			Hits []struct {
				ID int `json:"id"`
			} `json:"hits"`
			HitsTruncated bool     `json:"hits_truncated"`
			NextOffset    *int     `json:"next_offset"`
			NextMaxHits   *int     `json:"next_max_hits"`
			Errors        []string `json:"errors"`
		}
		raw := callSuccess(t, session, "search", args)
		if err := json.Unmarshal([]byte(raw), &page); err != nil || len(raw) > 256*1024 || len(page.Hits) != 2 ||
			page.Hits[0].ID != 0 || page.Hits[1].ID != 1 || !page.HitsTruncated ||
			page.NextOffset == nil || *page.NextOffset != 2 || page.NextMaxHits == nil || *page.NextMaxHits != 3 ||
			!reflect.DeepEqual(page.Errors, []string{"one split failed"}) || !strings.Contains(raw, `"order_id":9007199254740993`) {
			t.Fatalf("first page: length=%d hits=%v truncated=%v next=%v size=%v errors=%v unmarshal=%v", len(raw), page.Hits, page.HitsTruncated, page.NextOffset, page.NextMaxHits, page.Errors, err)
		}
		args["start_offset"], args["max_hits"] = *page.NextOffset, *page.NextMaxHits
		raw = callSuccess(t, session, "search", args)
		if err := json.Unmarshal([]byte(raw), &page); err != nil || len(raw) > 256*1024 || len(page.Hits) != 2 ||
			page.Hits[0].ID != 2 || page.Hits[1].ID != 3 || page.HitsTruncated || page.NextOffset != nil || page.NextMaxHits != nil {
			t.Fatalf("final page: length=%d hits=%v truncated=%v next=%v size=%v unmarshal=%v", len(raw), page.Hits, page.HitsTruncated, page.NextOffset, page.NextMaxHits, err)
		}
	})

	t.Run("paginates truncated hits even when Quickwit undercounts", func(t *testing.T) {
		t.Parallel()
		hit := `{"log":"` + strings.Repeat("x", 100*1024) + `"}`
		fake := &fakeQuickwit{index: indexMetadata, result: `{"num_hits":1,"hits":[` + hit + `,` + hit + `,` + hit + `],"errors":["one split failed"]}`}
		session := newSession(t, fake)
		args := validSearch()
		args["max_hits"] = 3
		result := callSuccess(t, session, "search", args)
		if len(result) > 256*1024 || !strings.Contains(result, `"hits_truncated":true`) || !strings.Contains(result, `"next_offset":2`) {
			t.Errorf("undercounted page: length=%d", len(result))
		}
	})

	t.Run("rejects oversized individual hits or search metadata", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name, result, message string
			maxHits               int
		}{
			{"single hit", `{"num_hits":1,"hits":[{"log":"` + strings.Repeat("<", 50000) + `"}]}`, "single search hit exceeds", 1},
			{"metadata", `{"num_hits":0,"hits":[],"errors":["` + strings.Repeat("x", 256*1024) + `"]}`, "search result metadata exceeds", 0},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				session := newSession(t, &fakeQuickwit{index: indexMetadata, result: tc.result})
				args := validSearch()
				args["max_hits"] = tc.maxHits
				callError(t, session, "search", args, tc.message)
			})
		}
	})

	t.Run("sorts oldest first and applies defaults", func(t *testing.T) {
		t.Parallel()
		fake := &fakeQuickwit{index: indexMetadata, result: searchResult}
		session := newSession(t, fake)
		args := validSearch()
		args["oldest_first"] = true
		callSuccess(t, session, "search", args)
		body := fake.recorded()[1].body
		if body["sort_by"] != "-timestamp" || body["max_hits"] != float64(20) || body["start_offset"] != float64(0) {
			t.Errorf("search body = %v", body)
		}
	})

	t.Run("pagination boundaries", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name   string
			result string
			args   map[string]any
		}{
			{"count only", `{"num_hits":3,"hits":[],"errors":[]}`, map[string]any{"max_hits": 0}},
			{"last page", `{"num_hits":3,"hits":[{}],"errors":[]}`, map[string]any{"start_offset": 2}},
			{"pagination ceiling", `{"num_hits":10001,"hits":[{}],"errors":[]}`, map[string]any{"start_offset": 9999, "max_hits": 1}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				fake := &fakeQuickwit{index: indexMetadata, result: tc.result}
				session := newSession(t, fake)
				args := validSearch()
				for k, v := range tc.args {
					args[k] = v
				}
				result := callSuccess(t, session, "search", args)
				if !strings.Contains(result, `"next_offset":null`) || !strings.Contains(result, `"next_max_hits":null`) {
					t.Errorf("result = %s", result)
				}
				if tc.name == "count only" && fake.recorded()[1].body["max_hits"] != float64(0) {
					t.Errorf("explicit zero replaced by default")
				}
			})
		}
	})

	t.Run("paginates to the ceiling using the advertised next page size", func(t *testing.T) {
		t.Parallel()
		type request struct {
			MaxHits     int `json:"max_hits"`
			StartOffset int `json:"start_offset"`
		}
		requests := make(chan request, 2)
		session := newSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/indexes/logs-test" {
				fmt.Fprint(w, indexMetadata)
				return
			}
			if r.URL.Path != "/api/v1/logs-test/search" || r.Method != http.MethodPost {
				http.NotFound(w, r)
				return
			}
			var req request
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			requests <- req
			_ = json.NewEncoder(w).Encode(map[string]any{
				"num_hits": 10005, "hits": make([]struct{}, req.MaxHits), "errors": []any{},
			})
		}))
		args := validSearch()
		args["start_offset"], args["max_hits"] = 9970, 20
		var page struct {
			Hits        []json.RawMessage `json:"hits"`
			NextOffset  *int              `json:"next_offset"`
			NextMaxHits *int              `json:"next_max_hits"`
		}
		raw := callSuccess(t, session, "search", args)
		if err := json.Unmarshal([]byte(raw), &page); err != nil || len(page.Hits) != 20 ||
			page.NextOffset == nil || *page.NextOffset != 9990 || page.NextMaxHits == nil || *page.NextMaxHits != 10 {
			t.Fatalf("first page = %s; error = %v", raw, err)
		}
		args["start_offset"], args["max_hits"] = *page.NextOffset, *page.NextMaxHits
		raw = callSuccess(t, session, "search", args)
		if err := json.Unmarshal([]byte(raw), &page); err != nil || len(page.Hits) != 10 ||
			page.NextOffset != nil || page.NextMaxHits != nil {
			t.Fatalf("final page = %s; error = %v", raw, err)
		}
		if first, second := <-requests, <-requests; first != (request{20, 9970}) || second != (request{10, 9990}) {
			t.Errorf("upstream requests = %+v, %+v", first, second)
		}
	})

	t.Run("sorts indexes by exact ID", func(t *testing.T) {
		t.Parallel()
		indexA := strings.Replace(indexMetadata, "logs-test", "logs-a", 1)
		indexZ := strings.Replace(indexMetadata, "logs-test", "logs-z", 1)
		session := newSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "[%s,%s]", indexZ, indexA)
		}))
		result := callSuccess(t, session, "list_indexes", map[string]any{})
		if strings.Index(result, "logs-a") >= strings.Index(result, "logs-z") {
			t.Errorf("indexes not sorted: %s", result)
		}
	})

	t.Run("accepts boundary values without rounding", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name  string
			start int64
			end   int64
			query string
		}{
			{"full 24 hours and unicode limit", 0, 86400, strings.Repeat("é", 4096)},
			{"large timestamps", 9007199254740993, 9007199254741000, "*"},
			{"maximum int64", 9223372036854775747, 9223372036854775807, "*"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				bodies := make(chan []byte, 1)
				session := newSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						fmt.Fprint(w, indexMetadata)
						return
					}
					body, _ := io.ReadAll(r.Body)
					bodies <- body
					fmt.Fprint(w, `{"num_hits":0,"hits":[],"errors":[]}`)
				}))
				args := validSearch()
				args["start_timestamp"], args["end_timestamp"], args["query"] = tc.start, tc.end, tc.query
				callSuccess(t, session, "search", args)
				var body struct {
					Start int64  `json:"start_timestamp"`
					End   int64  `json:"end_timestamp"`
					Query string `json:"query"`
				}
				if err := json.Unmarshal(<-bodies, &body); err != nil || body.Start != tc.start || body.End != tc.end || body.Query != tc.query {
					t.Fatalf("request values changed: %+v, error %v", body, err)
				}
			})
		}
	})

	t.Run("cancels an in-flight upstream request", func(t *testing.T) {
		t.Parallel()
		started, canceled := make(chan struct{}), make(chan struct{})
		session := newSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			select {
			case <-r.Context().Done():
				close(canceled)
			case <-time.After(5 * time.Second):
			}
		}))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		returned := make(chan error, 1)
		go func() {
			_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_indexes", Arguments: map[string]any{}})
			returned <- err
		}()
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("upstream request did not start")
		}
		cancel()
		select {
		case <-canceled:
		case <-time.After(3 * time.Second):
			t.Fatal("upstream request was not canceled")
		}
		if err := <-returned; err == nil {
			t.Error("canceled call unexpectedly succeeded")
		}
	})

	t.Run("rejects invalid input before HTTP", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name, key string
			value     any
		}{
			{"path", "index_id", "../ingest"}, {"wildcard", "index_id", "logs-*"}, {"reserved index", "index_id", "_elastic"}, {"newline", "index_id", "valid\n"},
			{"empty query", "query", ""}, {"blank query", "query", " "}, {"long query", "query", strings.Repeat("a", 4097)}, {"long unicode query", "query", strings.Repeat("é", 4097)},
			{"too many hits", "max_hits", 101}, {"negative hits", "max_hits", -1}, {"fractional hits", "max_hits", 1.5}, {"decimal integer", "max_hits", json.Number("1.0")},
			{"offset limit", "start_offset", 10000}, {"offset too large", "start_offset", 10001}, {"negative offset", "start_offset", -1},
			{"negative start", "start_timestamp", -1}, {"end equals start", "end_timestamp", 100}, {"reversed time", "end_timestamp", 99}, {"window too long", "end_timestamp", 86501},
			{"null hits", "max_hits", nil}, {"invalid boolean", "oldest_first", "true"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				fake := &fakeQuickwit{index: indexMetadata, result: searchResult}
				session := newSession(t, fake)
				args := validSearch()
				args[tc.key] = tc.value
				callError(t, session, "search", args, "")
				if reqs := fake.recorded(); len(reqs) != 0 {
					t.Errorf("unexpected HTTP requests: %+v", reqs)
				}
			})
		}
	})

	t.Run("rejects unknown arguments before HTTP", func(t *testing.T) {
		t.Parallel()
		searchArgs := validSearch()
		searchArgs["max_hit"] = 0
		for _, tc := range []struct {
			tool string
			args map[string]any
		}{
			{"list_indexes", map[string]any{"name_contain": "TEST"}},
			{"get_index_schema", map[string]any{"index_id": "logs-test", "extra": true}},
			{"search", searchArgs},
		} {
			t.Run(tc.tool, func(t *testing.T) {
				t.Parallel()
				fake := &fakeQuickwit{index: indexMetadata, result: searchResult}
				session := newSession(t, fake)
				callError(t, session, tc.tool, tc.args, "")
				if reqs := fake.recorded(); len(reqs) != 0 {
					t.Errorf("unexpected HTTP requests: %+v", reqs)
				}
			})
		}
	})

	t.Run("rejects missing required arguments", func(t *testing.T) {
		t.Parallel()
		for _, key := range []string{"index_id", "query", "start_timestamp", "end_timestamp"} {
			t.Run(key, func(t *testing.T) {
				t.Parallel()
				fake := &fakeQuickwit{index: indexMetadata}
				session := newSession(t, fake)
				args := validSearch()
				delete(args, key)
				callError(t, session, "search", args, "")
				if len(fake.recorded()) != 0 {
					t.Error("invalid input reached Quickwit")
				}
			})
		}
	})

	t.Run("rejects searches without a timestamp field", func(t *testing.T) {
		t.Parallel()
		fake := &fakeQuickwit{index: strings.Replace(indexMetadata, `"timestamp_field":"timestamp"`, `"timestamp_field":null`, 1)}
		session := newSession(t, fake)
		callError(t, session, "search", validSearch(), "no timestamp field")
		if len(fake.recorded()) != 1 {
			t.Errorf("requests = %+v", fake.recorded())
		}
	})

	t.Run("reports backend failures", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name            string
			status          int
			result, message string
		}{
			{"invalid JSON", 200, "not JSON", "invalid JSON"},
			{"HTTP error", 400, `{"message":"unknown field: missing"}`, "unknown field: missing"},
			{"redirect", 307, `{}`, "HTTP 307"},
			{"oversized response", 200, `{"value":"` + strings.Repeat("x", 4*1024*1024) + `"}`, "response exceeds 4 MiB"},
			{"invalid hits", 200, `{"num_hits":3,"hits":null}`, "invalid search hits"},
			{"invalid hit count", 200, `{"num_hits":null,"hits":[]}`, "invalid search hit count"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				fake := &fakeQuickwit{index: indexMetadata, status: tc.status, result: tc.result}
				session := newSession(t, fake)
				callError(t, session, "search", validSearch(), tc.message)
				for _, req := range fake.recorded() {
					if req.path == "/must-not-be-followed" {
						t.Error("followed redirect")
					}
				}
			})
		}
	})

	t.Run("reports network failure", func(t *testing.T) {
		t.Parallel()
		httpServer := httptest.NewServer(&fakeQuickwit{index: indexMetadata})
		session := sessionForURL(t, httpServer.URL)
		httpServer.Close()
		callError(t, session, "list_indexes", map[string]any{}, "Quickwit request failed")
	})
}

func validSearch() map[string]any {
	return map[string]any{"index_id": "logs-test", "query": `log:"ORDER_ID"`, "start_timestamp": 100, "end_timestamp": 200}
}

func newSession(t *testing.T, handler http.Handler) *mcp.ClientSession {
	t.Helper()
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return sessionForURL(t, httpServer.URL)
}

func sessionForURL(t *testing.T, origin string) *mcp.ClientSession {
	t.Helper()
	server, err := quickwitmcp.NewServer(origin)
	if err != nil {
		t.Fatal(err)
	}
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func call(t *testing.T, session *mcp.ClientSession, name string, args any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func resultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if len(result.Content) != 1 {
		t.Fatalf("content = %+v", result.Content)
	}
	content, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("not text content: %+v", result.Content[0])
	}
	return content.Text
}

func callSuccess(t *testing.T, session *mcp.ClientSession, name string, args any) string {
	t.Helper()
	result := call(t, session, name, args)
	text := resultText(t, result)
	if result.IsError {
		t.Fatalf("%s failed: %s", name, text)
	}
	if result.StructuredContent == nil {
		t.Fatalf("%s has no structured content", name)
	}
	return text
}

func callError(t *testing.T, session *mcp.ClientSession, name string, args any, contains string) {
	t.Helper()
	result := call(t, session, name, args)
	text := resultText(t, result)
	if !result.IsError {
		t.Fatalf("%s succeeded: %s", name, text)
	}
	if !strings.Contains(text, contains) {
		t.Errorf("error %q does not contain %q", text, contains)
	}
}
