// Package dockerdrv runs runners as local Docker containers instead of cloud VMs.
//
// This is not a mock. It implements the full cloud.Provider contract against a
// real Docker daemon with real create/get/delete/list semantics, which means an
// end-to-end test that passes here exercises every part of the controller except
// the remote cloud's API calls. It exists so runnerforge can be developed and
// tested without spending money or waiting for VMs to boot.
//
// It talks to the Engine API directly over the daemon socket rather than pulling
// in the full Docker client library, because the six calls needed here are
// simple and the dependency is not.
package dockerdrv

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/slop-place/runnerforge/internal/cloud"
)

func init() { cloud.Register("docker", New) }

// Driver provisions containers on a Docker daemon.
type Driver struct {
	http *http.Client
	// host is the API base. For unix sockets this is a placeholder authority;
	// the dialer ignores it.
	host string
}

// New builds a Docker driver. Recognised settings:
//
//	socket: path to the daemon socket (default: $DOCKER_HOST, else the usual paths)
func New(cfg map[string]any) (cloud.Provider, error) {
	sock, _ := cfg["socket"].(string)
	if sock == "" {
		sock = socketFromEnv()
	}
	if sock == "" {
		return nil, fmt.Errorf("docker: no daemon socket found; set DOCKER_HOST or the socket setting")
	}
	d := &Driver{host: "http://docker"}
	d.http = &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
		},
	}
	return d, nil
}

// socketFromEnv resolves the daemon socket the way the docker CLI does, with
// the addition of OrbStack's and Colima's paths which are common on macOS.
func socketFromEnv() string {
	if h := os.Getenv("DOCKER_HOST"); strings.HasPrefix(h, "unix://") {
		return strings.TrimPrefix(h, "unix://")
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		"/var/run/docker.sock",
		filepath.Join(home, ".orbstack/run/docker.sock"),
		filepath.Join(home, ".docker/run/docker.sock"),
		filepath.Join(home, ".colima/default/docker.sock"),
	} {
		if fi, err := os.Stat(p); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return p
		}
	}
	return ""
}

func (d *Driver) Name() string { return "docker" }

func (d *Driver) Capabilities() cloud.Capabilities {
	return cloud.Capabilities{
		// Containers start a process directly, so the runner credential arrives
		// as environment and arguments rather than cloud-init.
		CredentialMode: cloud.CredentialEnv,
		Tags:           true, // container labels
		SecurityGroups: false,
		PublicIPv4:     false,
		TypicalBoot:    2 * time.Second,
	}
}

// ---- Engine API plumbing ----

func (d *Driver) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, d.host+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return fmt.Errorf("docker %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return cloud.ErrNotFound
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		msg := e.Message
		if msg == "" {
			msg = strings.TrimSpace(string(data))
		}
		return fmt.Errorf("docker %s %s: %s: %s", method, path, resp.Status, msg)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// ---- provider ----

type createBody struct {
	Image      string            `json:"Image"`
	User       string            `json:"User,omitempty"`
	Entrypoint []string          `json:"Entrypoint,omitempty"`
	Cmd        []string          `json:"Cmd,omitempty"`
	Env        []string          `json:"Env,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
	HostConfig hostConfig        `json:"HostConfig"`
}

type hostConfig struct {
	NanoCPUs    int64    `json:"NanoCpus,omitempty"`
	Memory      int64    `json:"Memory,omitempty"`
	AutoRemove  bool     `json:"AutoRemove"`
	Privileged  bool     `json:"Privileged,omitempty"`
	Binds       []string `json:"Binds,omitempty"`
	NetworkMode string   `json:"NetworkMode,omitempty"`
	ExtraHosts  []string `json:"ExtraHosts,omitempty"`
}

func (d *Driver) Provision(ctx context.Context, req cloud.ProvisionRequest) (*cloud.Instance, error) {
	// The forge decides which runner image to use, so its choice wins; the
	// cloud's image catalogue is the fallback for operators who pin one.
	img := req.Bootstrap.ContainerImage
	if img == "" {
		img = cloud.SpecString(req.ImageSpec, "image")
	}
	if img == "" {
		return nil, fmt.Errorf("docker: no container image for %s (set the pool's container_image, "+
			"or give the cloud image %q a spec of {\"image\": \"...\"})", req.Name, req.Image)
	}

	labels := map[string]string{}
	for k, v := range req.Tags {
		labels[k] = v
	}
	for k, v := range req.Owner.Tags() {
		labels[k] = v // owner keys always win
	}
	labels[cloud.TagCreatedAt] = time.Now().UTC().Format(time.RFC3339)

	env := make([]string, 0, len(req.Bootstrap.Env))
	for k, v := range req.Bootstrap.Env {
		env = append(env, k+"="+v)
	}

	hc := hostConfig{
		// AutoRemove is deliberately off. If the daemon reaped exited
		// containers for us, List would stop reporting machines that still
		// need accounting, and the reaper would go blind exactly when a job
		// has just failed.
		AutoRemove: false,
	}
	applySize(&hc, req)

	body := createBody{
		Image:      img,
		User:       cloud.SpecString(req.SizeSpec, "user"),
		Entrypoint: req.Bootstrap.Entrypoint,
		Cmd:        req.Bootstrap.Args,
		Env:        env,
		Labels:     labels,
		HostConfig: hc,
	}

	// Pull on demand if the image is not present locally.
	if err := d.ensureImage(ctx, img); err != nil {
		return nil, err
	}

	var created struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	q := url.Values{"name": {req.Name}}
	if err := d.do(ctx, http.MethodPost, "/containers/create?"+q.Encode(), body, &created); err != nil {
		return nil, err
	}
	if err := d.do(ctx, http.MethodPost, "/containers/"+created.ID+"/start", nil, nil); err != nil {
		// Roll back so a failed start does not leave an accounted-for husk.
		_ = d.Delete(ctx, created.ID)
		return nil, fmt.Errorf("docker: start %s: %w", req.Name, err)
	}
	return &cloud.Instance{
		ID:        created.ID,
		Name:      req.Name,
		State:     cloud.StateCreating,
		CreatedAt: time.Now().UTC(),
		Tags:      labels,
	}, nil
}

// applySize maps a size spec onto container limits. Absent values mean
// "unlimited", which is the right default for local development.
//
// Recognised spec keys: cpus (fractional cores), memory_mb, privileged,
// docker_socket (bind-mounts the daemon socket so jobs can build images).
func applySize(hc *hostConfig, req cloud.ProvisionRequest) {
	if f, ok := cloud.SpecFloat(req.SizeSpec, "cpus"); ok && f > 0 {
		hc.NanoCPUs = int64(f * 1e9)
	}
	if mb, ok := cloud.SpecInt(req.SizeSpec, "memory_mb"); ok && mb > 0 {
		hc.Memory = mb * 1024 * 1024
	}
	if cloud.SpecBool(req.SizeSpec, "privileged") {
		hc.Privileged = true
	}
	// Forgejo and GitLab runners execute job steps in containers of their own,
	// so without the daemon socket a container-mode runner cannot run anything.
	if sock := cloud.SpecString(req.SizeSpec, "docker_socket"); sock != "" {
		hc.Binds = append(hc.Binds, sock+":/var/run/docker.sock")
	}
	if net := cloud.SpecString(req.SizeSpec, "network"); net != "" {
		hc.NetworkMode = net
	}
}

func (d *Driver) ensureImage(ctx context.Context, img string) error {
	err := d.do(ctx, http.MethodGet, "/images/"+url.PathEscape(img)+"/json", nil, nil)
	if err == nil {
		return nil
	}
	if err != cloud.ErrNotFound {
		return err
	}
	name, tag := img, "latest"
	if i := strings.LastIndex(img, ":"); i > strings.LastIndex(img, "/") {
		name, tag = img[:i], img[i+1:]
	}
	q := url.Values{"fromImage": {name}, "tag": {tag}}
	// Pulling streams progress; we only care that it completes.
	return d.do(ctx, http.MethodPost, "/images/create?"+q.Encode(), nil, nil)
}

type inspectResp struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Status   string `json:"Status"`
		Running  bool   `json:"Running"`
		ExitCode int    `json:"ExitCode"`
		Error    string `json:"Error"`
	} `json:"State"`
	Created         string `json:"Created"`
	NetworkSettings struct {
		IPAddress string `json:"IPAddress"`
	} `json:"NetworkSettings"`
}

func (d *Driver) Get(ctx context.Context, id string) (*cloud.Instance, error) {
	var r inspectResp
	if err := d.do(ctx, http.MethodGet, "/containers/"+id+"/json", nil, &r); err != nil {
		return nil, err
	}
	return fromInspect(&r), nil
}

func fromInspect(r *inspectResp) *cloud.Instance {
	created, _ := time.Parse(time.RFC3339Nano, r.Created)
	inst := &cloud.Instance{
		ID:        r.ID,
		Name:      strings.TrimPrefix(r.Name, "/"),
		PrivateIP: r.NetworkSettings.IPAddress,
		CreatedAt: created,
		Tags:      r.Config.Labels,
	}
	switch {
	case r.State.Running:
		inst.State = cloud.StateRunning
	case r.State.Status == "created":
		inst.State = cloud.StateCreating
	case r.State.ExitCode != 0:
		inst.State = cloud.StateError
		inst.Err = fmt.Sprintf("container exited with code %d: %s", r.State.ExitCode, r.State.Error)
	default:
		inst.State = cloud.StateStopped
	}
	return inst
}

func (d *Driver) Delete(ctx context.Context, id string) error {
	q := url.Values{"force": {"1"}, "v": {"1"}}
	err := d.do(ctx, http.MethodDelete, "/containers/"+id+"?"+q.Encode(), nil, nil)
	if err == cloud.ErrNotFound {
		// Already gone is success: teardown must converge, not wedge.
		return nil
	}
	return err
}

// Logs returns the container's combined output.
//
// The Engine API multiplexes stdout and stderr into framed chunks when the
// container has no TTY, so the stream is de-framed here rather than handed back
// with 8-byte headers embedded in the text.
func (d *Driver) Logs(ctx context.Context, id string, tail int) (string, error) {
	if tail <= 0 {
		tail = 200
	}
	q := url.Values{
		"stdout": {"1"}, "stderr": {"1"},
		"tail": {strconv.Itoa(tail)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		d.host+"/containers/"+id+"/logs?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	resp, err := d.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", cloud.ErrNotFound
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("docker: logs %s: %s: %s", id, resp.Status, strings.TrimSpace(string(raw)))
	}
	return deframe(raw), nil
}

// deframe strips Docker's stream multiplexing headers.
//
// Each frame is an 8-byte header (stream byte, three reserved, then a big-endian
// length) followed by that many bytes of payload. Output from a TTY container
// has no framing at all, so anything that does not parse as frames is returned
// unchanged.
func deframe(b []byte) string {
	var out strings.Builder
	for len(b) >= 8 {
		if b[0] > 2 || b[1] != 0 || b[2] != 0 || b[3] != 0 {
			return string(b) // not framed
		}
		n := int(binary.BigEndian.Uint32(b[4:8]))
		if n < 0 || 8+n > len(b) {
			return string(b)
		}
		out.Write(b[8 : 8+n])
		b = b[8+n:]
	}
	if out.Len() == 0 {
		return string(b)
	}
	return out.String()
}

func (d *Driver) List(ctx context.Context, owner cloud.Owner) ([]*cloud.Instance, error) {
	filters := map[string][]string{
		"label": {cloud.TagController + "=" + owner.Controller},
	}
	if owner.Pool != "" {
		filters["label"] = append(filters["label"], cloud.TagPool+"="+owner.Pool)
	}
	fb, err := json.Marshal(filters)
	if err != nil {
		return nil, err
	}
	// all=1 matters: a container that exited still holds a name and still needs
	// to be destroyed, so it must appear here.
	q := url.Values{"all": {"1"}, "filters": {string(fb)}}

	var raw []struct {
		ID      string            `json:"Id"`
		Names   []string          `json:"Names"`
		Labels  map[string]string `json:"Labels"`
		State   string            `json:"State"`
		Status  string            `json:"Status"`
		Created int64             `json:"Created"`
	}
	if err := d.do(ctx, http.MethodGet, "/containers/json?"+q.Encode(), nil, &raw); err != nil {
		return nil, err
	}
	out := make([]*cloud.Instance, 0, len(raw))
	for _, c := range raw {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		st := cloud.StateStopped
		switch c.State {
		case "running":
			st = cloud.StateRunning
		case "created":
			st = cloud.StateCreating
		case "dead":
			st = cloud.StateError
		}
		out = append(out, &cloud.Instance{
			ID:        c.ID,
			Name:      name,
			State:     st,
			CreatedAt: time.Unix(c.Created, 0).UTC(),
			Tags:      c.Labels,
		})
	}
	return out, nil
}
