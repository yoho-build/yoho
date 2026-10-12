package composefile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestServiceNames(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		env   map[string]string
		dest  string
		want  string
	}{
		{
			name: "mixed profiles",
			files: map[string]string{"compose.yaml": `
services:
  web: {image: nginx}
  db: {image: postgres}
  debug: {image: busybox, profiles: [debug]}
`},
			want: "db,web",
		},
		{
			name: "overlay adds a Service",
			files: map[string]string{
				"compose.yaml":         "services:\n  web: {image: nginx}\n",
				"compose.staging.yaml": "services:\n  worker: {image: busybox}\n",
			},
			dest: "staging",
			want: "web,worker",
		},
		{
			name: "overlay of another Destination ignored",
			files: map[string]string{
				"compose.yaml":         "services:\n  web: {image: nginx}\n",
				"compose.staging.yaml": "services:\n  worker: {image: busybox}\n",
			},
			dest: "production",
			want: "web",
		},
		{
			name:  "missing env_file",
			files: map[string]string{"compose.yaml": "services:\n  web: {image: nginx, env_file: [missing.env]}\n  db: {image: postgres}\n"},
			want:  "db,web",
		},
		{
			name:  "missing label_file",
			files: map[string]string{"compose.yaml": "services:\n  web: {image: nginx, label_file: missing.labels}\n"},
			want:  "web",
		},
		{
			name:  "invalid unrelated x-yoho",
			files: map[string]string{"compose.yaml": "services:\n  web:\n    image: nginx\n    x-yoho: {proxy: 42, nonsense: true}\n"},
			want:  "web",
		},
		{
			name:  "inconsistent depends_on",
			files: map[string]string{"compose.yaml": "services:\n  web: {image: nginx, depends_on: [ghost]}\n"},
			want:  "web",
		},
		{
			name:  "interpolated from env",
			files: map[string]string{"compose.yaml": "services:\n  web: {image: \"nginx:${TAG}\"}\n", ".env": "TAG=1\n"},
			env:   map[string]string{"TAG": "2"},
			want:  "web",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := writeFiles(t, c.files)
			got, err := ServiceNames(context.Background(), dir, nil, c.env, "shop-prod", c.dest)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, ",") != c.want {
				t.Errorf("got %v, want %s", got, c.want)
			}
		})
	}
}

// The strict Load rejects what ServiceNames tolerates.
func TestServiceNamesLenienterThanLoad(t *testing.T) {
	for _, body := range []string{
		"services:\n  web: {image: nginx, env_file: [missing.env]}\n",
		"services:\n  web: {image: nginx, label_file: missing.labels}\n",
		"services:\n  web: {image: nginx, depends_on: [ghost]}\n",
		"services:\n  web:\n    image: nginx\n    x-yoho: {proxy: 42}\n",
	} {
		dir := writeFiles(t, map[string]string{"compose.yaml": body})
		if _, err := Load(context.Background(), dir, nil, nil, "shop-prod", ""); err == nil {
			t.Errorf("Load accepted:\n%s", body)
		}
	}
}

func TestServiceNamesNoCompose(t *testing.T) {
	if _, err := ServiceNames(context.Background(), t.TempDir(), nil, nil, "shop-prod", ""); err == nil || !strings.Contains(err.Error(), "no compose file") {
		t.Errorf("err %v", err)
	}
}
