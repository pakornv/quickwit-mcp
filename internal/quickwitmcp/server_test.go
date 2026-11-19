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

	t.Run("searches and preserves partial errors and large numbers", func(t *testing.T) {
		t.Parallel()
		fake := &fakeQuickwit{index: indexMetadata, result: searchResult}
		session := newSession(t, fake)
		args := validSearch()
		args["max_hits"] = 1
		args["start_offset"] = 1
		result := callSuccess(t, session, "search", args)
		for _, fragment := range []string{`"next_offset":2`, `"order_id":9007199254740993`, `"one split failed"`} {
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
				if !strings.Contains(result, `"next_offset":null`) {
					t.Errorf("result = %s", result)
				}
				if tc.name == "count only" && fake.recorded()[1].body["max_hits"] != float64(0) {
					t.Errorf("explicit zero replaced by default")
				}
			})
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
