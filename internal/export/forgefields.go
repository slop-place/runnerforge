package export

import (
	"github.com/slop-place/runnerforge/internal/cloud"
	"github.com/slop-place/runnerforge/internal/forge"
)

// forgeImpl looks up a forge's declared fields.
//
// Wrapped so the renderers do not depend on the registry's shape, and so an
// unknown kind degrades to rendering every setting as plain rather than
// failing: a configuration that is slightly wrong is more useful than none.
func forgeImpl(kind string) []cloud.Field {
	impl, ok := forge.ByKind(forge.Kind(kind))
	if !ok {
		return nil
	}
	return impl.Fields
}
