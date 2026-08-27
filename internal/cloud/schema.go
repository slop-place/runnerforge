package cloud

import "context"

// FieldType selects the input a form renders for a setting.
type FieldType string

// The input kinds a driver can ask for.
const (
	FieldText     FieldType = "text"
	FieldPassword FieldType = "password"
	FieldNumber   FieldType = "number"
	FieldBool     FieldType = "bool"
	FieldSelect   FieldType = "select"
)

// Field describes one configurable setting.
//
// Drivers declare these so the UI can render a real form. Before this existed
// an operator configured a cloud by hand-writing a JSON object into a textarea,
// which is a config file wearing a costume: no labels, no validation, no idea
// which keys a driver even reads, and a Glance UUID typed from memory.
type Field struct {
	// Key is the name this value is stored under, and the name the driver
	// reads back out of its settings map.
	Key   string
	Label string
	Type  FieldType

	// Secret routes the value into the encrypted credentials column rather
	// than the settings column.
	Secret bool

	Required    bool
	Placeholder string
	// Help is shown under the input. Say what the value is for, not what it
	// is called; the label already says that.
	Help string
	// Default is pre-filled on a new record.
	Default string
	// Options populates a select.
	Options []Option
}

// Option is one choice in a select field.
type Option struct {
	Value string
	Label string
}

// Schema is everything a driver needs configured, in the order it should be
// presented.
type Schema struct {
	// Connection is the cloud account itself: endpoints, region, credentials.
	Connection []Field
	// Size describes one entry in the instance-size catalogue.
	Size []Field
	// Image describes one entry in the image catalogue.
	Image []Field
}

// Catalog is an optional Provider capability: listing what the account can
// actually build with.
//
// A driver that implements it turns the size and image forms from "type a UUID
// you looked up elsewhere" into a picker of what the account really offers.
type Catalog interface {
	// Flavors lists the machine types available in the configured region.
	Flavors(ctx context.Context) ([]CatalogItem, error)
	// Images lists the bootable images available in the configured region.
	Images(ctx context.Context) ([]CatalogItem, error)
}

// CatalogItem is one thing an account can build with.
type CatalogItem struct {
	// ID is what goes into the spec.
	ID string
	// Label is what an operator recognises.
	Label string
	// Detail is a short qualifier: vCPU and memory for a flavor, the login
	// user for an image.
	Detail string
}
