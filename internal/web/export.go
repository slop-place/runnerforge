package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/slop-place/runnerforge/internal/export"
)

// exportFormats are the ways a record can be taken away as code.
type exportFormat struct {
	Key   string
	Label string
	Lang  string
}

// The two ways a record can be taken away.
const (
	formatHCL = "hcl"
	formatCRD = "crd"
)

var exportFormats = []exportFormat{
	{Key: formatHCL, Label: "Terraform", Lang: "hcl"},
	{Key: formatCRD, Label: "Kubernetes", Lang: "yaml"},
}

// exportHandler renders one record as HCL or as a custom resource.
//
// The UI is not meant to be a dead end: someone who clicked through the forms to
// get a pool working can take the same thing away as code rather than rebuilding
// it by hand and hoping the two agree.
func (s *Server) exportRecord(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	format := r.PathValue("format")
	id := pathID(r)

	body, err := s.renderExport(r, kind, format, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	v := s.base(r, "Export", navFor(kind))
	v.Export = body
	v.ExportKind = kind
	v.ExportFormat = format
	v.ExportID = id
	v.ExportFormats = exportFormats
	s.render(w, "export", v)
}

// errUnknownExport is returned for a kind or format that does not exist.
var errUnknownExport = errors.New("nothing to export here")

func (s *Server) renderExport(r *http.Request, kind, format string, id uint) (string, error) {
	ctx := r.Context()
	switch kind {
	case "cloud":
		c, err := s.db.CloudByID(ctx, id)
		if err != nil {
			return "", err
		}
		if format == formatCRD {
			return export.CloudCRD(c), nil
		}
		return export.CloudHCL(c), nil
	case "forge":
		f, err := s.db.ForgeByID(ctx, id)
		if err != nil {
			return "", err
		}
		if format == formatCRD {
			return export.ForgeCRD(f), nil
		}
		return export.ForgeHCL(f), nil
	case "pool":
		p, err := s.db.PoolByID(ctx, id)
		if err != nil {
			return "", err
		}
		if format == formatCRD {
			return export.PoolCRD(p), nil
		}
		return export.PoolHCL(p), nil
	default:
		return "", fmt.Errorf("%w: %q", errUnknownExport, kind)
	}
}

// navFor keeps the right nav item highlighted on an export page.
func navFor(kind string) string {
	switch kind {
	case "cloud":
		return "clouds"
	case "forge":
		return "forges"
	default:
		return "pools"
	}
}

// exportAll renders the whole configuration, so an existing deployment can be
// adopted into code in one step rather than a record at a time.
func (s *Server) exportAll(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	format := r.PathValue("format")

	clouds, err := s.db.Clouds(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	forges, err := s.db.Forges(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pools, err := s.db.Pools(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var body strings.Builder
	sep := "\n"
	if format == formatCRD {
		sep = "---\n"
	}
	for i := range clouds {
		body.WriteString(pick(format, export.CloudHCL(&clouds[i]), export.CloudCRD(&clouds[i])) + sep)
	}
	for i := range forges {
		body.WriteString(pick(format, export.ForgeHCL(&forges[i]), export.ForgeCRD(&forges[i])) + sep)
	}
	for i := range pools {
		body.WriteString(pick(format, export.PoolHCL(&pools[i]), export.PoolCRD(&pools[i])) + sep)
	}

	v := s.base(r, "Export", "")
	v.Export = body.String()
	v.ExportKind = "all"
	v.ExportFormat = format
	v.ExportFormats = exportFormats
	s.render(w, "export", v)
}

func pick(format, hcl, crd string) string {
	if format == formatCRD {
		return crd
	}
	return hcl
}

// exportRaw serves the same content as a plain file, for piping straight into a
// repository.
func (s *Server) exportRaw(w http.ResponseWriter, r *http.Request) {
	body, err := s.renderExport(r, r.PathValue("kind"), r.PathValue("format"), pathID(r))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	name := r.PathValue("kind")
	ext := ".tf"
	if r.PathValue("format") == formatCRD {
		ext = ".yaml"
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "attachment; filename=\"runnerforge-"+name+ext+"\"")
	_, _ = w.Write([]byte(body))
}
