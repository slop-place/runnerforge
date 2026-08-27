package openstack

import (
	"context"
	"fmt"
	"sort"

	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"

	"github.com/slop-place/runnerforge/internal/cloud"
)

const (
	// imageListLimit bounds one page of the image catalogue.
	imageListLimit = 500
	// mbPerGB converts a flavor's megabytes for display.
	mbPerGB = 1024
)

// Flavors lists the machine types the account can build in this region.
//
// This is what turns the size form from "paste a UUID you looked up in another
// tab" into a list of what the account actually offers, with the numbers an
// operator is choosing between.
func (d *Driver) Flavors(ctx context.Context) ([]cloud.CatalogItem, error) {
	pages, err := flavors.ListDetail(d.compute, flavors.ListOpts{}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("openstack: list flavors: %w", err)
	}
	all, err := flavors.ExtractFlavors(pages)
	if err != nil {
		return nil, fmt.Errorf("openstack: extract flavors: %w", err)
	}

	// Smallest first: the cheapest thing that works is usually the right
	// choice, and a long unsorted list is where a costly mis-click comes from.
	//
	// The flavors are sorted before being converted, because sorting the
	// converted slice while comparing the original one compares whichever
	// entries happen to be at those indices mid-sort, which is not an ordering
	// at all.
	sort.Slice(all, func(i, j int) bool {
		if all[i].VCPUs != all[j].VCPUs {
			return all[i].VCPUs < all[j].VCPUs
		}
		if all[i].RAM != all[j].RAM {
			return all[i].RAM < all[j].RAM
		}
		return all[i].Name < all[j].Name
	})

	out := make([]cloud.CatalogItem, 0, len(all))
	for _, f := range all {
		out = append(out, cloud.CatalogItem{
			ID:    f.ID,
			Label: f.Name,
			Detail: fmt.Sprintf("%d vCPU · %s RAM · %d GB disk",
				f.VCPUs, humanMB(f.RAM), f.Disk),
		})
	}
	return out, nil
}

// Images lists the bootable images available to the account.
func (d *Driver) Images(ctx context.Context) ([]cloud.CatalogItem, error) {
	if d.image == nil {
		return nil, errNoImageService
	}
	pages, err := images.List(d.image, images.ListOpts{
		Status: images.ImageStatusActive,
		Limit:  imageListLimit,
	}).AllPages(ctx)
	if err != nil {
		return nil, fmt.Errorf("openstack: list images: %w", err)
	}
	all, err := images.ExtractImages(pages)
	if err != nil {
		return nil, fmt.Errorf("openstack: extract images: %w", err)
	}

	out := make([]cloud.CatalogItem, 0, len(all))
	for _, img := range all {
		// Glance carries no login user — that is an OVHcloud APIv6 field — but
		// it does carry the distro family, which is what an operator is
		// actually choosing by, and which makes a Windows image obvious in a
		// list of runner images.
		out = append(out, cloud.CatalogItem{
			ID:     img.ID,
			Label:  img.Name,
			Detail: stringProp(img.Properties, "distro_family", "os_type"),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}

// stringProp returns the first of keys present as a string.
func stringProp(props map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := props[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// humanMB renders a megabyte count the way a price list would.
func humanMB(mb int) string {
	if mb >= mbPerGB && mb%mbPerGB == 0 {
		return fmt.Sprintf("%d GB", mb/mbPerGB)
	}
	return fmt.Sprintf("%d MB", mb)
}
