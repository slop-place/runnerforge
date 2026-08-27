// Package openstack provisions machines on any OpenStack cloud, including
// OVHcloud Public Cloud.
//
// OVHcloud exposes two APIs. Its own APIv6 can create instances and pass
// user-data, but it has no endpoint for server metadata — and the reaper claims
// machines by metadata, so a driver built on APIv6 could only ever fall back to
// matching on name. This driver therefore speaks OpenStack directly, which also
// makes it usable against any other OpenStack provider unchanged.
//
// One OVHcloud-specific note: the Keystone endpoint is per-brand rather than
// global (auth.cloud.ovh.us for US projects, auth.cloud.ovh.net for EU), so the
// auth URL is configuration rather than something this package assumes.
package openstack

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	gopenstack "github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"

	"github.com/slop-place/runnerforge/internal/cloud"
)

func init() { cloud.Register("openstack", New) }

// Driver provisions OpenStack servers.
type Driver struct {
	compute *gophercloud.ServiceClient
	region  string
	// defaultNetwork is attached when a size or image does not name one. OVHcloud
	// calls its public network "Ext-Net".
	defaultNetwork string
}

// New builds an OpenStack driver. Recognised settings:
//
//	auth_url    Keystone v3 endpoint (required; e.g. https://auth.cloud.ovh.us/v3)
//	region      region name (required; e.g. US-EAST-VA-1)
//	project_id  project/tenant id (required)
//	username    OpenStack user (required)
//	password    that user's password (required)
//	domain      user and project domain (default "Default")
//	network     network to attach (default "Ext-Net")
func New(cfg map[string]any) (cloud.Provider, error) {
	get := func(k string) string { s, _ := cfg[k].(string); return strings.TrimSpace(s) }

	missing := make([]string, 0, 5)
	for _, k := range []string{"auth_url", "region", "project_id", "username", "password"} {
		if get(k) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("openstack: missing required settings: %s", strings.Join(missing, ", "))
	}

	domain := get("domain")
	if domain == "" {
		domain = "Default"
	}
	network := get("network")
	if network == "" {
		network = "Ext-Net"
	}

	opts := gophercloud.AuthOptions{
		IdentityEndpoint: strings.TrimRight(get("auth_url"), "/") + "/",
		Username:         get("username"),
		Password:         get("password"),
		DomainName:       domain,
		TenantID:         get("project_id"),
		AllowReauth:      true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	provider, err := gopenstack.AuthenticatedClient(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("openstack: authenticate: %w", err)
	}
	compute, err := gopenstack.NewComputeV2(provider, gophercloud.EndpointOpts{Region: get("region")})
	if err != nil {
		return nil, fmt.Errorf("openstack: compute endpoint for region %q: %w", get("region"), err)
	}
	// Server tags and metadata-on-create need a reasonably modern microversion.
	compute.Microversion = "2.52"

	return &Driver{compute: compute, region: get("region"), defaultNetwork: network}, nil
}

func (d *Driver) Name() string { return "openstack" }

func (d *Driver) Capabilities() cloud.Capabilities {
	return cloud.Capabilities{
		CredentialMode: cloud.CredentialCloudInit,
		Tags:           true, // server metadata
		SecurityGroups: true,
		PublicIPv4:     true,
		TypicalBoot:    60 * time.Second,
	}
}

func (d *Driver) Provision(ctx context.Context, req cloud.ProvisionRequest) (*cloud.Instance, error) {
	flavor := cloud.SpecString(req.SizeSpec, "flavor_id")
	if flavor == "" {
		flavor = cloud.SpecString(req.SizeSpec, "flavor")
	}
	if flavor == "" {
		return nil, fmt.Errorf("openstack: size %q has no flavor or flavor_id in its spec", req.Size)
	}
	image := cloud.SpecString(req.ImageSpec, "id")
	if image == "" {
		image = cloud.SpecString(req.ImageSpec, "image_id")
	}
	if image == "" {
		return nil, fmt.Errorf("openstack: image %q has no id in its spec", req.Image)
	}

	// Ownership metadata is what the reaper reads. It is set at creation so
	// there is no window in which a machine exists without being claimable.
	meta := map[string]string{}
	for k, v := range req.Tags {
		meta[k] = v
	}
	for k, v := range req.Owner.Tags() {
		meta[k] = v
	}
	meta[cloud.TagCreatedAt] = time.Now().UTC().Format(time.RFC3339)

	network := cloud.SpecString(req.SizeSpec, "network")
	if network == "" {
		network = d.defaultNetwork
	}

	opts := servers.CreateOpts{
		Name:      req.Name,
		FlavorRef: flavor,
		ImageRef:  image,
		Metadata:  meta,
		UserData:  req.Bootstrap.CloudInit,
	}
	// A network may be given by uuid or, as OVHcloud's public "Ext-Net" usually
	// is, by name. Leaving it unset lets the cloud pick, which is right for
	// projects that have exactly one network.
	switch {
	case cloud.SpecString(req.SizeSpec, "network_id") != "":
		opts.Networks = []servers.Network{{UUID: cloud.SpecString(req.SizeSpec, "network_id")}}
	case network != "":
		opts.Networks = []servers.Network{{UUID: network}}
	}
	if sg := cloud.SpecString(req.SizeSpec, "security_group"); sg != "" {
		opts.SecurityGroups = []string{sg}
	}

	srv, err := servers.Create(ctx, d.compute, opts, nil).Extract()
	if err != nil {
		return nil, fmt.Errorf("openstack: create server %s: %w", req.Name, err)
	}
	return &cloud.Instance{
		ID:        srv.ID,
		Name:      req.Name,
		State:     cloud.StateCreating,
		CreatedAt: srv.Created,
		Tags:      meta,
	}, nil
}

func (d *Driver) Get(ctx context.Context, id string) (*cloud.Instance, error) {
	srv, err := servers.Get(ctx, d.compute, id).Extract()
	if err != nil {
		if isNotFound(err) {
			return nil, cloud.ErrNotFound
		}
		return nil, fmt.Errorf("openstack: get server %s: %w", id, err)
	}
	return fromServer(srv), nil
}

func (d *Driver) Delete(ctx context.Context, id string) error {
	err := servers.Delete(ctx, d.compute, id).ExtractErr()
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("openstack: delete server %s: %w", id, err)
	}
	// Already gone is success: teardown must converge, not wedge.
	return nil
}

func (d *Driver) List(ctx context.Context, owner cloud.Owner) ([]*cloud.Instance, error) {
	pages, err := servers.List(d.compute, servers.ListOpts{}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("openstack: list servers: %w", err)
	}
	all, err := servers.ExtractServers(pages)
	if err != nil {
		return nil, fmt.Errorf("openstack: extract servers: %w", err)
	}
	out := make([]*cloud.Instance, 0, len(all))
	for i := range all {
		// Metadata comes back on the list response, so ownership is decided
		// here without a per-server round trip.
		if !owner.Matches(all[i].Metadata) {
			continue
		}
		out = append(out, fromServer(&all[i]))
	}
	return out, nil
}

// Logs returns the machine's console output.
//
// This is what makes a failed boot diagnosable: cloud-init writes to the
// console, so a machine that never registered a runner explains itself here.
func (d *Driver) Logs(ctx context.Context, id string, tail int) (string, error) {
	if tail <= 0 {
		tail = 200
	}
	out, err := servers.ShowConsoleOutput(ctx, d.compute, id, servers.ShowConsoleOutputOpts{
		Length: tail,
	}).Extract()
	if err != nil {
		if isNotFound(err) {
			return "", cloud.ErrNotFound
		}
		return "", fmt.Errorf("openstack: console output for %s: %w", id, err)
	}
	return out, nil
}

func fromServer(s *servers.Server) *cloud.Instance {
	inst := &cloud.Instance{
		ID:        s.ID,
		Name:      s.Name,
		CreatedAt: s.Created,
		Tags:      s.Metadata,
	}
	switch strings.ToUpper(s.Status) {
	case "ACTIVE":
		inst.State = cloud.StateRunning
	case "BUILD", "BUILDING", "REBUILD":
		inst.State = cloud.StateCreating
	case "SHUTOFF", "STOPPED", "SUSPENDED", "PAUSED", "SHELVED", "SHELVED_OFFLOADED":
		inst.State = cloud.StateStopped
	case "ERROR":
		inst.State = cloud.StateError
		if s.Fault.Message != "" {
			inst.Err = s.Fault.Message
		} else {
			inst.Err = "server is in ERROR state"
		}
	case "DELETED", "SOFT_DELETED":
		inst.State = cloud.StateGone
	default:
		// Transitional states (RESIZE, MIGRATING, …) are treated as running so
		// a machine mid-transition is not mistaken for finished work.
		inst.State = cloud.StateRunning
	}
	inst.PublicIP, inst.PrivateIP = addresses(s)
	return inst
}

// addresses picks a public and a private address out of the server's networks.
func addresses(s *servers.Server) (public, private string) {
	for _, ifaces := range s.Addresses {
		list, ok := ifaces.([]any)
		if !ok {
			continue
		}
		for _, entry := range list {
			m, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			addr, _ := m["addr"].(string)
			if addr == "" || strings.Contains(addr, ":") {
				continue // skip IPv6 here; runners only need one reachable v4
			}
			switch m["OS-EXT-IPS:type"] {
			case "floating":
				public = addr
			case "fixed":
				if isPrivate(addr) {
					if private == "" {
						private = addr
					}
				} else if public == "" {
					// OVHcloud's Ext-Net hands out routable addresses as fixed
					// IPs rather than floating ones.
					public = addr
				}
			}
		}
	}
	return public, private
}

func isPrivate(ip string) bool {
	return strings.HasPrefix(ip, "10.") ||
		strings.HasPrefix(ip, "192.168.") ||
		strings.HasPrefix(ip, "172.16.") || strings.HasPrefix(ip, "172.17.") ||
		strings.HasPrefix(ip, "172.18.") || strings.HasPrefix(ip, "172.19.") ||
		strings.HasPrefix(ip, "172.2") || strings.HasPrefix(ip, "172.30.") ||
		strings.HasPrefix(ip, "172.31.")
}

func isNotFound(err error) bool {
	var e gophercloud.ErrUnexpectedResponseCode
	if errors.As(err, &e) {
		return e.Actual == http.StatusNotFound
	}
	return gophercloud.ResponseCodeIs(err, http.StatusNotFound)
}
