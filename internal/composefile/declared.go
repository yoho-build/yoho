package composefile

import (
	"fmt"
	"os"

	"go.yaml.in/yaml/v3"
)

// Declared maps a Service or top-level volume name to the compose files
// that declare it.
type Declared struct {
	Services map[string][]string
	Volumes  map[string][]string
}

func addDeclared(m map[string][]string, name, file string) {
	for _, f := range m[name] {
		if f == file {
			return
		}
	}
	m[name] = append(m[name], file)
}

// DeclaredNames reads the top-level services and volumes keys of files (as
// Load resolves them against dir, default discovery when empty) and of their
// <file>.<destination><ext> overlays for destination. It parses the raw YAML,
// so profile-disabled Services count too. Missing files are skipped. into is
// filled in place so callers can collect several Destinations.
func DeclaredNames(into *Declared, dir string, files []string, destination string) error {
	if into.Services == nil {
		into.Services = map[string][]string{}
	}
	if into.Volumes == nil {
		into.Volumes = map[string][]string{}
	}
	paths, _, err := discover(dir, files, destination)
	if err != nil {
		return nil // no compose file yet: nothing declared
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		var doc struct {
			Services map[string]yaml.Node `yaml:"services"`
			Volumes  map[string]yaml.Node `yaml:"volumes"`
		}
		if err := yaml.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		for n := range doc.Services {
			addDeclared(into.Services, n, p)
		}
		for n := range doc.Volumes {
			addDeclared(into.Volumes, n, p)
		}
	}
	return nil
}
