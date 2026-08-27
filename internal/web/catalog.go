package web

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/slop-place/runnerforge/internal/cloud"
	"github.com/slop-place/runnerforge/internal/store"
)

// catalogTimeout bounds a listing so a slow cloud does not hang the edit page.
const catalogTimeout = 20 * time.Second

// errNoCatalog is returned for drivers that cannot enumerate what an account
// offers. Not a failure: those drivers just take a plain text field.
var errNoCatalog = errors.New("this driver does not list what the account offers")

// catalog lists the flavors and images a cloud account can build with.
//
// Failures are returned rather than swallowed so the edit page can say why the
// pickers are empty — "the credentials are wrong" and "this driver has no
// catalogue" need different responses from an operator.
func (s *Server) catalog(ctx context.Context, c *store.Cloud) ([]cloud.CatalogItem, []cloud.CatalogItem, error) {
	prov, err := s.ctrl.Provider(c)
	if err != nil {
		return nil, nil, err
	}
	cat, ok := prov.(cloud.Catalog)
	if !ok {
		return nil, nil, errNoCatalog
	}

	ctx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()

	flavors, err := cat.Flavors(ctx)
	if err != nil {
		return nil, nil, err
	}
	images, err := cat.Images(ctx)
	if err != nil {
		// Flavors alone are still worth showing.
		return flavors, nil, err
	}
	return flavors, images, nil
}

// specFromForm builds a size or image spec from the driver's declared fields.
func (s *Server) specFromForm(
	r *http.Request, cloudID uint, pick func(cloud.Schema) []cloud.Field,
) (store.Params, error) {
	c, err := s.db.CloudByID(r.Context(), cloudID)
	if err != nil {
		return nil, err
	}
	drv, ok := cloud.DriverByName(c.Driver)
	if !ok {
		return nil, errNoCatalog
	}
	// Specs hold no secrets, so the credentials half of the result is unused.
	spec, _, err := collectFields(r, pick(drv.Schema), nil)
	return spec, err
}
