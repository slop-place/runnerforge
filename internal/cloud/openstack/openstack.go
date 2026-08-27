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
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	gopenstack "github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/networks"

	"github.com/slop-place/runnerforge/internal/cloud"
)

func init() { cloud.Register("openstack", New) }

const (
	// authTimeout bounds the initial Keystone handshake.
	authTimeout = 30 * time.Second
	// novaMicroversion is the lowest version that supports server tags and
	// metadata on create, both of which the reaper depends on.
	novaMicroversion = "2.52"
	// typicalBootTime informs the controller's readiness expectations;
	// OVHcloud usually reaches ACTIVE well inside a minute.
	typicalBootTime = 60 * time.Second
	// defaultLogTail is how many console lines are fetched by default.
	defaultLogTail = 200
	// uuidLen is the length of a canonical UUID string.
	uuidLen = 36
	// requiredSettings is how many settings New insists on.
	requiredSettings = 5
)

// uuidDashPositions are the indices where a canonical UUID has its separators.
var uuidDashPositions = [4]int{8, 13, 18, 23}

// Driver provisions OpenStack servers.
type Driver struct {
	compute *gophercloud.ServiceClient
	network *gophercloud.ServiceClient
	region  string
	// defaultNetwork is attached when a size does not name one. OVHcloud calls
	// its public network "Ext-Net".
	defaultNetwork string

	// netIDs caches network name to uuid lookups. Nova will only accept a uuid,
	// but operators think in names, so names are resolved once and reused.
	netMu  sync.Mutex
	netIDs map[string]string
}

// New builds an OpenStack driver. Recognised settings:
//
//	auth_url    Keystone v3 endpoint (required; e.g. https://auth.cloud.ovh.us/v3)
//	region      name of the region (required; e.g. US-EAST-VA-1)
//	project_id  project/tenant id (required)
//	username    OpenStack user (required)
//	password    that user's password (required)
//	domain      user and project domain (default "Default")
//	network     name or uuid of the network to attach (default "Ext-Net")
func New(cfg map[string]any) (cloud.Provider, error) {
	get := func(k string) string { s, _ := cfg[k].(string); return strings.TrimSpace(s) }

	missing := make([]string, 0, requiredSettings)
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

	ctx, cancel := context.WithTimeout(context.Background(), authTimeout)
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
	compute.Microversion = novaMicroversion

	net, err := gopenstack.NewNetworkV2(provider, gophercloud.EndpointOpts{Region: get("region")})
	if err != nil {
		return nil, fmt.Errorf("openstack: network endpoint for region %q: %w", get("region"), err)
	}

	return &Driver{
		compute:        compute,
		network:        net,
		region:         get("region"),
		defaultNetwork: network,
		netIDs:         map[string]string{},
	}, nil
}

// Name returns the driver name as it appears in configuration.
func (d *Driver) Name() string { return "openstack" }

// Capabilities reports what this driver supports. Server metadata is the one
// that matters most: the reaper claims machines by it.
func (d *Driver) Capabilities() cloud.Capabilities {
	return cloud.Capabilities{
		CredentialMode: cloud.CredentialCloudInit,
		Tags:           true, // server metadata
		SecurityGroups: true,
		PublicIPv4:     true,
		TypicalBoot:    typicalBootTime,
	}
}

// Provision creates a server and returns as soon as the API accepts the call.
// It does not wait for the machine to boot; the controller polls Get for that.
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
	maps.Copy(meta, req.Tags)
	maps.Copy(meta, req.Owner.Tags()) // owner keys always win
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
	// is, by name. Nova only accepts a uuid, so a name is resolved first.
	// Leaving it unset lets the cloud pick, which is right for projects that
	// have exactly one network.
	switch {
	case cloud.SpecString(req.SizeSpec, "network_id") != "":
		opts.Networks = []servers.Network{{UUID: cloud.SpecString(req.SizeSpec, "network_id")}}
	case network != "":
		id, err := d.networkID(ctx, network)
		if err != nil {
			return nil, err
		}
		opts.Networks = []servers.Network{{UUID: id}}
	}
	if sg := cloud.SpecString(req.SizeSpec, "security_group"); sg != "" {
		opts.SecurityGroups = []string{sg}
	}

	srv, err := servers.Create(ctx, d.compute, opts, nil).Extract()
	if err != nil {
		return nil, fmt.Errorf("openstack: create server %s: %w", req.Name, err)
	}
	// Nova's create response does not populate Created. Leaving it zero would be
	// dangerous rather than merely untidy: the reaper ages machines off by this
	// timestamp, and a zero time reads as infinitely old.
	created := srv.Created
	if created.IsZero() {
		created = time.Now().UTC()
	}
	return &cloud.Instance{
		ID:        srv.ID,
		Name:      req.Name,
		State:     cloud.StateCreating,
		CreatedAt: created,
		Tags:      meta,
	}, nil
}

// Get reports a server's current state.
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

// Delete destroys a server. Deleting one that is already gone is success, so
// that a retried teardown converges instead of wedging.
func (d *Driver) Delete(ctx context.Context, id string) error {
	err := servers.Delete(ctx, d.compute, id).ExtractErr()
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("openstack: delete server %s: %w", id, err)
	}
	// Already gone is success: teardown must converge, not wedge.
	return nil
}

// List returns every server this deployment owns.
//
// This is the reaper's ground truth. Metadata comes back on the list response,
// so ownership is decided without a round trip per server.
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
		tail = defaultLogTail
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
func addresses(s *servers.Server) (string, string) {
	var public, private string
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
	if e, ok := errors.AsType[gophercloud.ErrUnexpectedResponseCode](err); ok {
		return e.Actual == http.StatusNotFound
	}
	return gophercloud.ResponseCodeIs(err, http.StatusNotFound)
}

// networkID resolves a network name to its uuid, caching the result.
//
// A value that already looks like a uuid is passed through untouched, so
// operators can configure either form.
func (d *Driver) networkID(ctx context.Context, name string) (string, error) {
	if looksLikeUUID(name) {
		return name, nil
	}
	d.netMu.Lock()
	if id, ok := d.netIDs[name]; ok {
		d.netMu.Unlock()
		return id, nil
	}
	d.netMu.Unlock()

	pages, err := networks.List(d.network, networks.ListOpts{Name: name}).AllPages(ctx)
	if err != nil {
		return "", fmt.Errorf("openstack: look up network %q: %w", name, err)
	}
	found, err := networks.ExtractNetworks(pages)
	if err != nil {
		return "", fmt.Errorf("openstack: extract networks: %w", err)
	}
	if len(found) == 0 {
		return "", fmt.Errorf("openstack: no network named %q in region %s", name, d.region)
	}

	d.netMu.Lock()
	d.netIDs[name] = found[0].ID
	d.netMu.Unlock()
	return found[0].ID, nil
}

// looksLikeUUID reports whether s has the shape of a UUID, so that a configured
// network can be either an id or a name without an extra setting to say which.
func looksLikeUUID(s string) bool {
	if len(s) != uuidLen {
		return false
	}
	for i, r := range s {
		switch i {
		case uuidDashPositions[0], uuidDashPositions[1], uuidDashPositions[2], uuidDashPositions[3]:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}
