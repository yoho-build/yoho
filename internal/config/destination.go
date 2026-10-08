package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// overlayExts are the Yoho file extensions accepted for a Destination overlay.
var overlayExts = []string{"yml", "yaml", "toml", "json", "jsonc"}

// LoadForDestination loads the Yoho file at path, resolves destination, and
// deep-merges an optional yoho.<destination>.<ext> overlay before decoding.
// destination may be empty; resolution then prefers production, then the only
// Destination. The returned overlay path is empty when no overlay file exists.
// Validate the result separately.
func LoadForDestination(path, destination string) (cfg *Config, dest string, overlay string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", "", err
	}
	generic, err := decodeGeneric(data)
	if err != nil {
		return nil, "", "", fmt.Errorf("%s: %w", path, err)
	}
	baseMap, ok := normalize(generic).(map[string]any)
	if !ok {
		return nil, "", "", fmt.Errorf("%s: top level must be a mapping", path)
	}
	var base Config
	if err := decodeStrict(baseMap, &base); err != nil {
		return nil, "", "", fmt.Errorf("%s: %w", path, err)
	}
	dest, err = base.DestName(destination)
	if err != nil {
		return nil, "", "", err
	}
	overlay, err = findDestinationOverlay(filepath.Dir(path), dest)
	if err != nil {
		return nil, "", "", err
	}
	merged := baseMap
	hint := path
	if overlay != "" {
		overMap, err := readGenericMap(overlay)
		if err != nil {
			return nil, "", "", err
		}
		merged = mergeMaps(baseMap, overMap)
		hint = overlay
	}
	var out Config
	if err := decodeStrict(merged, &out); err != nil {
		return nil, "", "", fmt.Errorf("%s: %w", hint, err)
	}
	out.applyDefaults()
	return &out, dest, overlay, nil
}

func readGenericMap(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	generic, err := decodeGeneric(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	m, ok := normalize(generic).(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: top level must be a mapping", path)
	}
	return m, nil
}

// findDestinationOverlay returns yoho.<destination>.<ext> next to the Yoho
// file. Several matches is an error; none is an empty path.
func findDestinationOverlay(dir, destination string) (string, error) {
	if destination == "" || strings.ContainsAny(destination, `/\`) || destination == "." || destination == ".." {
		return "", nil
	}
	var found []string
	for _, ext := range overlayExts {
		p := filepath.Join(dir, "yoho."+destination+"."+ext)
		st, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", err
		}
		if st.IsDir() {
			continue
		}
		found = append(found, p)
	}
	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("several Destination overlays for %q: %s; keep one", destination, strings.Join(found, ", "))
	}
}

// mergeMaps deep-merges over onto base. Maps merge recursively. Scalars and
// lists in over replace the base value.
func mergeMaps(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		if bv, ok := out[k]; ok {
			out[k] = mergeValues(bv, v)
			continue
		}
		out[k] = v
	}
	return out
}

func mergeValues(base, over any) any {
	bm, bok := asStringMap(base)
	om, ook := asStringMap(over)
	if bok && ook {
		return mergeMaps(bm, om)
	}
	return over
}

func asStringMap(v any) (map[string]any, bool) {
	m, ok := normalize(v).(map[string]any)
	return m, ok
}
