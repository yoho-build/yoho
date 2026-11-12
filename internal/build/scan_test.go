package build

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/ui"
)

func TestScanParsers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parse func([]byte) (scanCounts, error)
	}{{"trivy", parseTrivy}, {"grype", parseGrype}} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile("testdata/" + tc.name + ".json")
			if err != nil {
				t.Fatal(err)
			}
			got, err := tc.parse(data)
			if err != nil || got != (scanCounts{Critical: 1, High: 2}) {
				t.Fatalf("%+v %v", got, err)
			}
			if _, err := tc.parse([]byte("invalid")); err == nil {
				t.Fatal("accepted malformed JSON")
			}
			if got, err := tc.parse([]byte("{}")); err != nil || got != (scanCounts{}) {
				t.Fatalf("empty: %+v %v", got, err)
			}
		})
	}
}

func TestScanMissing(t *testing.T) {
	old := lookPath
	t.Cleanup(func() { lookPath = old })
	var names []string
	lookPath = func(name string) (string, error) { names = append(names, name); return "", errors.New("missing") }
	var out bytes.Buffer
	ScanImages(context.Background(), []string{"app:v1"}, "docker", "linux/amd64", ui.New(&out, ui.Human, false), func(context.Context, Command) error { t.Fatal("ran scanner"); return nil })
	if !strings.Contains(out.String(), "install trivy or grype") || !reflect.DeepEqual(names, []string{"trivy", "grype"}) {
		t.Fatal(out.String(), names)
	}
}

func TestScanCommandsAndWarnings(t *testing.T) {
	old := lookPath
	t.Cleanup(func() { lookPath = old })
	for _, scanner := range []string{"trivy", "grype"} {
		for _, fail := range []bool{false, true} {
			lookPath = func(name string) (string, error) {
				if name != scanner {
					return "", errors.New("missing")
				}
				return name, nil
			}
			var out bytes.Buffer
			ScanImages(context.Background(), []string{"app:v1"}, "docker", "linux/amd64", ui.New(&out, ui.JSON, false), func(_ context.Context, c Command) error {
				want := []string{"trivy", "image", "--quiet", "--ignore-unfixed", "--severity", "HIGH,CRITICAL", "--format", "json", "--image-src", "docker", "app:v1", "--platform", "linux/amd64"}
				if scanner == "grype" {
					want = []string{"grype", "docker:app:v1", "--only-fixed", "-o", "json", "--platform", "linux/amd64"}
				}
				if !reflect.DeepEqual(c.Argv, want) {
					t.Fatalf("argv %v", c.Argv)
				}
				if fail {
					return errors.New("sensitive output")
				}
				data, _ := os.ReadFile("testdata/" + scanner + ".json")
				_, err := c.Stdout.Write(data)
				return err
			})
			found := false
			for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
				var event map[string]any
				if err := json.Unmarshal([]byte(line), &event); err != nil {
					t.Fatal(err)
				}
				if event["event"] != "image.scan" {
					continue
				}
				found = true
				if event["image"] != "app:v1" || event["scanner"] != scanner || event["ok"] != !fail {
					t.Fatal(event)
				}
				critical, high := float64(1), float64(2)
				if fail {
					critical, high = 0, 0
				}
				if event["critical"] != critical || event["high"] != high {
					t.Fatal(event)
				}
			}
			if !found {
				t.Fatal(out.String())
			}
			if strings.Contains(out.String(), "sensitive output") {
				t.Fatal(out.String())
			}
		}
	}
}

func TestScanContainerArchiveCleanup(t *testing.T) {
	for _, scanner := range []string{"trivy", "grype"} {
		for _, fail := range []bool{false, true} {
			var archive string
			calls := 0
			counts, err := scanImage(context.Background(), "app:v1", "container", "linux/amd64", scanner, scanner, func(_ context.Context, c Command) error {
				calls++
				if calls == 1 {
					if !reflect.DeepEqual(c.Argv[:6], []string{"container", "image", "save", "--platform", "linux/amd64", "--output"}) || c.Argv[7] != "app:v1" {
						t.Fatal(c.Argv)
					}
					archive = c.Argv[6]
					if err := os.WriteFile(archive, []byte("archive"), 0600); err != nil {
						t.Fatal(err)
					}
					return nil
				}
				if _, err := os.Stat(archive); err != nil {
					t.Fatal(err)
				}
				if scanner == "trivy" && !reflect.DeepEqual(c.Argv[len(c.Argv)-4:], []string{"--input", archive, "--platform", "linux/amd64"}) {
					t.Fatal(c.Argv)
				}
				if scanner == "grype" && c.Argv[1] != "oci-archive:"+archive {
					t.Fatal(c.Argv)
				}
				if fail {
					return errors.New("scanner failed")
				}
				data, _ := os.ReadFile("testdata/" + scanner + ".json")
				_, err := c.Stdout.Write(data)
				return err
			})
			if calls != 2 || (err != nil) != fail {
				t.Fatalf("calls %d error %v", calls, err)
			}
			if !fail && counts != (scanCounts{Critical: 1, High: 2}) {
				t.Fatal(counts)
			}
			if _, err := os.Stat(archive); !os.IsNotExist(err) {
				t.Fatalf("temporary archive remains: %v", err)
			}
		}
	}
}
