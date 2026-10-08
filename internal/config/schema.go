package config

import (
	"encoding/json"
	"reflect"

	"github.com/invopop/jsonschema"
)

func reflector() *jsonschema.Reflector {
	return &jsonschema.Reflector{
		Anonymous:                  true,
		RequiredFromJSONSchemaTags: true,
	}
}

func marshalSchema(t reflect.Type, title string) ([]byte, error) {
	s := reflector().ReflectFromType(t)
	s.Version = "https://json-schema.org/draft/2020-12/schema"
	s.Title = title
	return json.MarshalIndent(s, "", "  ")
}

// Schema returns the JSON Schema (draft 2020-12) of the Yoho file.
func Schema() ([]byte, error) {
	return marshalSchema(reflect.TypeOf(Config{}), "Yoho file")
}

// ServiceExtSchema returns the JSON Schema of the compose `x-yoho` extension.
func ServiceExtSchema() ([]byte, error) {
	return marshalSchema(reflect.TypeOf(ServiceExt{}), "Yoho x-yoho service extension")
}
