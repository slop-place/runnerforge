// Package provider implements the Terraform provider for runnerforge.
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// requestTimeout bounds a single call to runnerforge.
	requestTimeout = 60 * time.Second
	// httpErrorFloor is the status at or above which a response is a failure.
	httpErrorFloor = 300
)

// ErrNotFound means the resource is gone.
//
// Terraform needs to tell "deleted outside Terraform" apart from "the API is
// broken": the first removes the resource from state, the second must fail the
// plan rather than silently proposing to recreate it.
var ErrNotFound = errors.New("not found")

// Client talks to a runnerforge deployment's JSON API.
type Client struct {
	endpoint string
	token    string
	http     *http.Client
}

// NewClient builds a client.
func NewClient(endpoint, token string) *Client {
	return &Client{
		endpoint: strings.TrimRight(endpoint, "/"),
		token:    token,
		http:     &http.Client{Timeout: requestTimeout},
	}
}

// Cloud mirrors runnerforge's cloud record.
type Cloud struct {
	ID       int64          `json:"id,omitempty"`
	Name     string         `json:"name"`
	Driver   string         `json:"driver"`
	Enabled  *bool          `json:"enabled,omitempty"`
	Settings map[string]any `json:"settings,omitempty"`
}

// Size mirrors an entry in a cloud's size catalogue.
type Size struct {
	ID        int64          `json:"id,omitempty"`
	CloudID   int64          `json:"cloud_id"`
	Name      string         `json:"name"`
	Spec      map[string]any `json:"spec,omitempty"`
	VCPUs     int64          `json:"vcpus,omitempty"`
	MemoryMB  int64          `json:"memory_mb,omitempty"`
	DiskGB    int64          `json:"disk_gb,omitempty"`
	HourlyUSD float64        `json:"hourly_usd,omitempty"`
}

// Image mirrors an entry in a cloud's image catalogue.
type Image struct {
	ID                 int64          `json:"id,omitempty"`
	CloudID            int64          `json:"cloud_id"`
	Name               string         `json:"name"`
	Spec               map[string]any `json:"spec,omitempty"`
	Username           string         `json:"username,omitempty"`
	PreinstalledDocker bool           `json:"preinstalled_docker,omitempty"`
	Notes              string         `json:"notes,omitempty"`
}

// Forge mirrors a forge connection.
type Forge struct {
	ID       int64          `json:"id,omitempty"`
	Name     string         `json:"name"`
	Kind     string         `json:"kind"`
	Enabled  *bool          `json:"enabled,omitempty"`
	Settings map[string]any `json:"settings,omitempty"`
}

// Pool mirrors a pool.
type Pool struct {
	ID             int64    `json:"id,omitempty"`
	Name           string   `json:"name"`
	Enabled        *bool    `json:"enabled,omitempty"`
	ForgeID        int64    `json:"forge_id"`
	CloudID        int64    `json:"cloud_id"`
	SizeID         int64    `json:"size_id"`
	ImageID        *int64   `json:"image_id,omitempty"`
	Labels         []string `json:"labels"`
	MinIdle        int64    `json:"min_idle"`
	MaxInstances   int64    `json:"max_instances"`
	JobTimeoutSec  int64    `json:"job_timeout_sec"`
	MaxLifetimeSec int64    `json:"max_lifetime_sec"`
	ContainerImage string   `json:"container_image,omitempty"`
	PublicIPv4     *bool    `json:"public_ipv4,omitempty"`
	AllowSSHFrom   []string `json:"allow_ssh_from,omitempty"`
}

func id(v int64) string { return strconv.FormatInt(v, 10) }

// The CRUD calls. Every resource follows the same shape, so the provider's
// resources stay thin.

// CreateCloud creates a cloud.
func (c *Client) CreateCloud(ctx context.Context, in Cloud) (Cloud, error) {
	var out Cloud
	err := c.do(ctx, http.MethodPost, "/api/v1/clouds", in, &out)
	return out, err
}

// GetCloud fetches a cloud.
func (c *Client) GetCloud(ctx context.Context, cloudID int64) (Cloud, error) {
	var out Cloud
	err := c.do(ctx, http.MethodGet, "/api/v1/clouds/"+id(cloudID), nil, &out)
	return out, err
}

// UpdateCloud updates a cloud.
func (c *Client) UpdateCloud(ctx context.Context, cloudID int64, in Cloud) (Cloud, error) {
	var out Cloud
	err := c.do(ctx, http.MethodPut, "/api/v1/clouds/"+id(cloudID), in, &out)
	return out, err
}

// DeleteCloud deletes a cloud.
func (c *Client) DeleteCloud(ctx context.Context, cloudID int64) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/clouds/"+id(cloudID), nil, nil)
}

// CreateSize creates a size catalogue entry.
func (c *Client) CreateSize(ctx context.Context, in Size) (Size, error) {
	var out Size
	err := c.do(ctx, http.MethodPost, "/api/v1/sizes", in, &out)
	return out, err
}

// GetSize fetches a size catalogue entry.
func (c *Client) GetSize(ctx context.Context, sizeID int64) (Size, error) {
	var out Size
	err := c.do(ctx, http.MethodGet, "/api/v1/sizes/"+id(sizeID), nil, &out)
	return out, err
}

// UpdateSize updates a size catalogue entry.
func (c *Client) UpdateSize(ctx context.Context, sizeID int64, in Size) (Size, error) {
	var out Size
	err := c.do(ctx, http.MethodPut, "/api/v1/sizes/"+id(sizeID), in, &out)
	return out, err
}

// DeleteSize deletes a size catalogue entry.
func (c *Client) DeleteSize(ctx context.Context, sizeID int64) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/sizes/"+id(sizeID), nil, nil)
}

// CreateImage creates an image catalogue entry.
func (c *Client) CreateImage(ctx context.Context, in Image) (Image, error) {
	var out Image
	err := c.do(ctx, http.MethodPost, "/api/v1/images", in, &out)
	return out, err
}

// GetImage fetches an image catalogue entry.
func (c *Client) GetImage(ctx context.Context, imageID int64) (Image, error) {
	var out Image
	err := c.do(ctx, http.MethodGet, "/api/v1/images/"+id(imageID), nil, &out)
	return out, err
}

// UpdateImage updates an image catalogue entry.
func (c *Client) UpdateImage(ctx context.Context, imageID int64, in Image) (Image, error) {
	var out Image
	err := c.do(ctx, http.MethodPut, "/api/v1/images/"+id(imageID), in, &out)
	return out, err
}

// DeleteImage deletes an image catalogue entry.
func (c *Client) DeleteImage(ctx context.Context, imageID int64) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/images/"+id(imageID), nil, nil)
}

// CreateForge creates a forge connection.
func (c *Client) CreateForge(ctx context.Context, in Forge) (Forge, error) {
	var out Forge
	err := c.do(ctx, http.MethodPost, "/api/v1/forges", in, &out)
	return out, err
}

// GetForge fetches a forge connection.
func (c *Client) GetForge(ctx context.Context, forgeID int64) (Forge, error) {
	var out Forge
	err := c.do(ctx, http.MethodGet, "/api/v1/forges/"+id(forgeID), nil, &out)
	return out, err
}

// UpdateForge updates a forge connection.
func (c *Client) UpdateForge(ctx context.Context, forgeID int64, in Forge) (Forge, error) {
	var out Forge
	err := c.do(ctx, http.MethodPut, "/api/v1/forges/"+id(forgeID), in, &out)
	return out, err
}

// DeleteForge deletes a forge connection.
func (c *Client) DeleteForge(ctx context.Context, forgeID int64) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/forges/"+id(forgeID), nil, nil)
}

// CreatePool creates a pool.
func (c *Client) CreatePool(ctx context.Context, in Pool) (Pool, error) {
	var out Pool
	err := c.do(ctx, http.MethodPost, "/api/v1/pools", in, &out)
	return out, err
}

// GetPool fetches a pool.
func (c *Client) GetPool(ctx context.Context, poolID int64) (Pool, error) {
	var out Pool
	err := c.do(ctx, http.MethodGet, "/api/v1/pools/"+id(poolID), nil, &out)
	return out, err
}

// UpdatePool updates a pool.
func (c *Client) UpdatePool(ctx context.Context, poolID int64, in Pool) (Pool, error) {
	var out Pool
	err := c.do(ctx, http.MethodPut, "/api/v1/pools/"+id(poolID), in, &out)
	return out, err
}

// DeletePool deletes a pool.
func (c *Client) DeletePool(ctx context.Context, poolID int64) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/pools/"+id(poolID), nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, rdr)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= httpErrorFloor {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		msg := e.Error
		if msg == "" {
			msg = strings.TrimSpace(string(data))
		}
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, msg)
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
