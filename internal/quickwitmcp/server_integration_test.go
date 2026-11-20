//go:build integration

package quickwitmcp_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNewServer_Integration(t *testing.T) {
	origin := os.Getenv("QUICKWIT_TEST_URL")
	if origin == "" {
		t.Fatal("QUICKWIT_TEST_URL must point to the disposable Quickwit instance; run scripts/test-quickwit.sh")
	}
	binary := filepath.Join(t.TempDir(), "quickwit-mcp")
	if output, err := exec.Command("go", "build", "-o", binary, "../../cmd/quickwit-mcp").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(), "QUICKWIT_URL="+origin)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "quickwit-integration", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	indexes := callSuccess(t, session, "list_indexes", map[string]any{"name_contains": "MCP-TEST"})
	if !strings.Contains(indexes, `"index_id":"quickwit-mcp-test"`) {
		t.Fatalf("index not discovered: %s", indexes)
	}
	t.Run("filters indexes on Quickwit by prefix", func(t *testing.T) {
		indexes := callSuccess(t, session, "list_indexes", map[string]any{"name_prefix": "quickwit-mcp", "name_contains": "MCP-TEST"})
		if !strings.Contains(indexes, `"index_id":"quickwit-mcp-test"`) {
			t.Errorf("filtered indexes = %s", indexes)
		}
		indexes = callSuccess(t, session, "list_indexes", map[string]any{"name_prefix": "Quickwit-mcp"})
		if indexes != `{"indexes":[]}` {
			t.Errorf("prefix should be case-sensitive: %s", indexes)
		}
	})
	schema := callSuccess(t, session, "get_index_schema", map[string]any{"index_id": "quickwit-mcp-test"})
	var index struct {
		IndexID    string `json:"index_id"`
		DocMapping struct {
			TimestampField string `json:"timestamp_field"`
		} `json:"doc_mapping"`
	}
	if err := json.Unmarshal([]byte(schema), &index); err != nil || index.IndexID != "quickwit-mcp-test" || index.DocMapping.TimestampField != "timestamp" {
		t.Fatalf("schema = %s; error = %v", schema, err)
	}

	for _, tc := range []struct {
		name        string
		oldestFirst bool
		want        []string
	}{
		{"newest first", false, []string{"second", "first"}},
		{"oldest first", true, []string{"first", "second"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := callSuccess(t, session, "search", map[string]any{
				"index_id": "quickwit-mcp-test", "query": "*",
				"start_timestamp": 1700000000, "end_timestamp": 1700000120,
				"oldest_first": tc.oldestFirst,
			})
			var result struct {
				NumHits int `json:"num_hits"`
				Hits    []struct {
					Log string `json:"log"`
				} `json:"hits"`
				Errors []any `json:"errors"`
			}
			if err := json.Unmarshal([]byte(raw), &result); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, hit := range result.Hits {
				got = append(got, hit.Log)
			}
			if result.NumHits != 2 || len(result.Errors) != 0 || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("result = %s; want 2 hits in order %v", raw, tc.want)
			}
		})
	}

	t.Run("follows the advertised next page", func(t *testing.T) {
		args := map[string]any{
			"index_id": "quickwit-mcp-test", "query": "*", "start_timestamp": 1700000000,
			"end_timestamp": 1700000120, "max_hits": 1, "oldest_first": true,
		}
		var page struct {
			Hits []struct {
				Log string `json:"log"`
			} `json:"hits"`
			NextOffset  *int `json:"next_offset"`
			NextMaxHits *int `json:"next_max_hits"`
		}
		raw := callSuccess(t, session, "search", args)
		if err := json.Unmarshal([]byte(raw), &page); err != nil || len(page.Hits) != 1 || page.Hits[0].Log != "first" ||
			page.NextOffset == nil || *page.NextOffset != 1 || page.NextMaxHits == nil || *page.NextMaxHits != 1 {
			t.Fatalf("first page = %s; error = %v", raw, err)
		}
		args["start_offset"], args["max_hits"] = *page.NextOffset, *page.NextMaxHits
		raw = callSuccess(t, session, "search", args)
		if err := json.Unmarshal([]byte(raw), &page); err != nil || len(page.Hits) != 1 || page.Hits[0].Log != "second" ||
			page.NextOffset != nil || page.NextMaxHits != nil {
			t.Errorf("final page = %s; error = %v", raw, err)
		}
	})

	raw := callSuccess(t, session, "search", map[string]any{
		"index_id": "quickwit-mcp-test", "query": "*",
		"start_timestamp": 1700000000, "end_timestamp": 1700000120, "max_hits": 0,
	})
	var count struct {
		NumHits     int               `json:"num_hits"`
		Hits        []json.RawMessage `json:"hits"`
		NextOffset  *int              `json:"next_offset"`
		NextMaxHits *int              `json:"next_max_hits"`
	}
	if err := json.Unmarshal([]byte(raw), &count); err != nil || count.NumHits != 2 || len(count.Hits) != 0 || count.NextOffset != nil || count.NextMaxHits != nil {
		t.Errorf("count-only result = %s; error = %v", raw, err)
	}
}
