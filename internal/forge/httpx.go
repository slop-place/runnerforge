package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPError carries a forge's HTTP failure with enough detail to act on.
type HTTPError struct {
	Status int
	Method string
	URL    string
	Body   string
}

func (e *HTTPError) Error() string {
	b := strings.TrimSpace(e.Body)
	if len(b) > 300 {
		b = b[:300] + "…"
	}
	return fmt.Sprintf("%s %s: %d: %s", e.Method, e.URL, e.Status, b)
}

// Client is a small JSON HTTP helper shared by the forge implementations.
//
// Each forge authenticates differently, so the auth header is supplied by the
// constructor rather than being baked in here.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	// Header is applied to every request; used for authorization.
	Header http.Header
}

// NewClient builds a client for a forge base URL.
func NewClient(base string, header http.Header) *Client {
	return &Client{
		BaseURL: strings.TrimRight(base, "/"),
		HTTP:    &http.Client{Timeout: 30 * time.Second},
		Header:  header,
	}
}

// Do performs a JSON request. A 404 is translated to ErrNotFound so callers can
// treat "already gone" as success during teardown.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	url := c.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	for k, vs := range c.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= 300 {
		return &HTTPError{Status: resp.StatusCode, Method: method, URL: url, Body: string(data)}
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: decode response: %w", method, url, err)
		}
	}
	return nil
}
