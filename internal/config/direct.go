package config

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/invopop/jsonschema"
)

// BuilderDirect is builder.direct: "auto", "true" or "false". YAML and TOML
// booleans decode to "true" and "false"; empty means auto.
type BuilderDirect string

// Values of BuilderDirect.
const (
	DirectAuto BuilderDirect = "auto"
	DirectOn   BuilderDirect = "true"
	DirectOff  BuilderDirect = "false"
)

// Mode returns the setting with the default applied.
func (d BuilderDirect) Mode() BuilderDirect {
	if d == "" {
		return DirectAuto
	}
	return d
}

// UnmarshalJSON accepts true, false, "auto", "true" and "false".
func (d *BuilderDirect) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	switch string(data) {
	case "null":
		*d = ""
		return nil
	case "true", "false":
		*d = BuilderDirect(data)
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("builder.direct must be auto, true or false")
	}
	switch BuilderDirect(s) {
	case "", DirectAuto, DirectOn, DirectOff:
		*d = BuilderDirect(s)
		return nil
	}
	return fmt.Errorf("builder.direct %q must be auto, true or false", s)
}

// JSONSchema describes both accepted forms.
func (BuilderDirect) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Description: "location=remote: ship images from the Builder straight to the Servers (auto, true or false)",
		OneOf: []*jsonschema.Schema{
			{Type: "boolean"},
			{Type: "string", Enum: []any{"auto", "true", "false"}},
		},
	}
}
