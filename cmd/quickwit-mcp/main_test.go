package main_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestBinary(t *testing.T) {
	t.Parallel()
	binary := filepath.Join(t.TempDir(), "quickwit-mcp")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}

	t.Run("communicates over stdio", func(t *testing.T) {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/v1/indexes":
				fmt.Fprint(w, `[{"index_config":{"index_id":"logs-test","doc_mapping":{"timestamp_field":"timestamp"}}}]`)
			case "/api/v1/indexes/logs-test":
				fmt.Fprint(w, `{"index_config":{"index_id":"logs-test","doc_mapping":{"timestamp_field":"timestamp"}}}`)
			case "/api/v1/logs-test/search":
				fmt.Fprint(w, `{"num_hits":1,"hits":[{"order_id":9007199254740993}],"errors":[]}`)
			default:
				http.NotFound(w, r)
			}
		}))
		defer backend.Close()

		cmd := exec.Command(binary)
		cmd.Env = append(os.Environ(), "QUICKWIT_URL="+backend.URL)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		client := mcp.NewClient(&mcp.Implementation{Name: "stdio-test", Version: "1"}, nil)
		session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		tools, err := session.ListTools(ctx, nil)
		if err != nil || len(tools.Tools) != 3 {
			t.Fatalf("list tools = %+v, %v", tools, err)
		}
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_indexes", Arguments: map[string]any{}})
		if err != nil || result.IsError {
			t.Fatalf("list_indexes = %+v, %v", result, err)
		}
		encoded, err := json.Marshal(result.StructuredContent)
		if err != nil || !strings.Contains(string(encoded), `"index_id":"logs-test"`) {
			t.Fatalf("structured content = %s, %v", encoded, err)
		}
		schema, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_index_schema", Arguments: map[string]any{"index_id": "logs-test"}})
		if err != nil || schema.IsError {
			t.Fatalf("get_index_schema = %+v, %v", schema, err)
		}
		result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "search", Arguments: map[string]any{
			"index_id": "logs-test", "query": "*", "start_timestamp": 100, "end_timestamp": 200,
		}})
		if err != nil || result.IsError || len(result.Content) != 1 {
			t.Fatalf("search = %+v, %v", result, err)
		}
		content, ok := result.Content[0].(*mcp.TextContent)
		if !ok || !strings.Contains(content.Text, `"order_id":9007199254740993`) {
			t.Fatalf("large number changed in search response: %+v", result.Content)
		}
	})

	t.Run("reports invalid configuration on stderr", func(t *testing.T) {
		cmd := exec.Command(binary)
		cmd.Env = append(os.Environ(), "QUICKWIT_URL=http://user:secret@example.com")
		output, err := cmd.Output()
		if err == nil || len(output) != 0 {
			t.Fatalf("expected failure with empty stdout; output=%q error=%v", output, err)
		}
		exitErr, ok := err.(*exec.ExitError)
		if !ok || !strings.Contains(string(exitErr.Stderr), "QUICKWIT_URL") || strings.Contains(string(exitErr.Stderr), "secret") {
			t.Fatalf("unexpected stderr: %v", err)
		}
	})
}
