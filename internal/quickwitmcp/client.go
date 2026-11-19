package quickwitmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxResponseBytes = 4 * 1024 * 1024

type client struct {
	baseURL string
	http    *http.Client
}

func newClient(baseURL string) (*client, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	u, err := url.Parse(baseURL)
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") ||
		u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" ||
		u.ForceQuery || strings.Contains(baseURL, "#") {
		return nil, fmt.Errorf("QUICKWIT_URL must be an HTTP(S) origin without credentials or a path")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &client{
		baseURL: baseURL,
		http: &http.Client{
			Transport: transport,
			Timeout:   15 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (c *client) request(ctx context.Context, path string, body any) (json.RawMessage, error) {
	var payload io.Reader
	method := http.MethodGet
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode Quickwit request: %w", err)
		}
		method = http.MethodPost
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/api/v1/"+path, payload)
	if err != nil {
		return nil, fmt.Errorf("create Quickwit request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Quickwit request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, err := io.ReadAll(io.LimitReader(resp.Body, 2000))
		if err != nil {
			return nil, fmt.Errorf("read Quickwit HTTP %d error: %w", resp.StatusCode, err)
		}
		return nil, fmt.Errorf("Quickwit HTTP %d: %s", resp.StatusCode, detail)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read Quickwit response: %w", err)
	}
	if len(raw) > maxResponseBytes {
		return nil, fmt.Errorf("Quickwit response exceeds 4 MiB; request fewer hits or a narrower scope")
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("Quickwit returned invalid JSON")
	}
	return raw, nil
}

type indexMetadata struct {
	IndexConfig *struct {
		IndexID        string          `json:"index_id"`
		DocMapping     json.RawMessage `json:"doc_mapping"`
		SearchSettings struct {
			DefaultSearchFields json.RawMessage `json:"default_search_fields"`
		} `json:"search_settings"`
	} `json:"index_config"`
}

func (m indexMetadata) mapping() (map[string]json.RawMessage, error) {
	if m.IndexConfig == nil || m.IndexConfig.IndexID == "" {
		return nil, fmt.Errorf("Quickwit returned invalid index metadata")
	}
	var mapping map[string]json.RawMessage
	if err := json.Unmarshal(m.IndexConfig.DocMapping, &mapping); err != nil || mapping == nil {
		return nil, fmt.Errorf("Quickwit returned invalid index mapping")
	}
	return mapping, nil
}

func timestampField(mapping map[string]json.RawMessage) (string, error) {
	var field *string
	if raw, ok := mapping["timestamp_field"]; ok {
		if err := json.Unmarshal(raw, &field); err != nil {
			return "", fmt.Errorf("Quickwit returned invalid timestamp field")
		}
	}
	if field == nil {
		return "", nil
	}
	return *field, nil
}

func (c *client) index(ctx context.Context, indexID string) (indexMetadata, map[string]json.RawMessage, error) {
	raw, err := c.request(ctx, "indexes/"+indexID, nil)
	if err != nil {
		return indexMetadata{}, nil, err
	}
	var metadata indexMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return indexMetadata{}, nil, fmt.Errorf("Quickwit returned invalid index metadata")
	}
	mapping, err := metadata.mapping()
	return metadata, mapping, err
}
