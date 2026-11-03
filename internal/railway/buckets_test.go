package railway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBucketsInventoryGolden(t *testing.T) {
	dir := filepath.Join("..", "..", "examples", "quickstart", "railway")
	in := filepath.Join(dir, "inventory-buckets.json")
	data, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	d, err := Draft(Request{Filename: in, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Buckets) != 2 || d.Buckets[0].Target != TargetRustFS || d.Buckets[1].Target != TargetExternal {
		t.Fatalf("buckets %+v", d.Buckets)
	}
	if join(d.Generated) != "POSTGRES_PASSWORD,RUSTFS_ACCESS_KEY,RUSTFS_SECRET_KEY" {
		t.Fatalf("generated %v", d.Generated)
	}
	if join(d.SecretKeys) != "APP_GREETING,S3_ACCESS_KEY_ID,S3_SECRET_ACCESS_KEY" {
		t.Fatalf("secret keys %v", d.SecretKeys)
	}
	if !contains(d.Services, "rustfs") || !contains(d.Volumes, "rustfsdata") {
		t.Fatalf("services %v volumes %v", d.Services, d.Volumes)
	}
	all := strings.Join(d.Checklist, "\n")
	for _, want := range []string{
		"rclone sync railway:shop-uploads yoho:shop-uploads --checksum",
		"rclone check railway:shop-uploads yoho:shop-uploads",
		"rclone sync railway:shop-media target:shop-media --checksum",
		"rclone check railway:shop-media target:shop-media",
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("checklist lacks %q:\n%s", want, all)
		}
	}
	for _, line := range d.Checklist {
		// Values reach rclone through the environment only.
		if strings.HasPrefix(line, "rclone ") && strings.Contains(line, "$(") {
			t.Fatalf("credential in argv: %s", line)
		}
	}
	assertGolden(t, filepath.Join(dir, "compose.buckets.draft.yaml"), d.Compose)
	assertGolden(t, filepath.Join(dir, "yoho.buckets.draft.yml"), d.Yoho)
}

func TestBucketsValidation(t *testing.T) {
	base := `{"project":"shop","services":[{"name":"web","image":"app:1"}],"buckets":[%s]}`
	cases := map[string]struct{ bucket, want string }{
		"unknown field": {`{"name":"media","size":"1GB","access_key":"x"}`, "unknown field"},
		"bad name":      {`{"name":"Media_Files","size":"1GB"}`, "not an S3 bucket name"},
		"bad size":      {`{"name":"media","size":"lots"}`, "is not a size"},
		"missing size":  {`{"name":"media"}`, "is not a size"},
		"bad target":    {`{"name":"media","size":"1GB","target":"minio"}`, "not rustfs or external"},
		"duplicate":     {`{"name":"media","size":"1GB"},{"name":"media","size":"2GB"}`, "duplicate bucket"},
		"secret value":  {`{"name":"media","size":"1GB","secret":"abc"}`, "holds a value"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Draft(Request{Filename: "inventory.json", Data: []byte(strings.Replace(base, "%s", tc.bucket, 1))})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v", err)
			}
		})
	}
	// A Service already named rustfs blocks the rustfs target, not external.
	clash := `{"project":"shop","services":[{"name":"rustfs","image":"app:1"}],"buckets":[{"name":"media","size":"1GB"}]}`
	if _, err := Draft(Request{Filename: "inventory.json", Data: []byte(clash)}); err == nil || !strings.Contains(err.Error(), "rustfs") {
		t.Fatalf("clash: %v", err)
	}
	ext := strings.Replace(clash, `"size":"1GB"`, `"size":"1GB","target":"external"`, 1)
	if _, err := Draft(Request{Filename: "inventory.json", Data: []byte(ext)}); err != nil {
		t.Fatalf("external: %v", err)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"12GB": 12e9, "1.5 TB": 15e11, "512MiB": 512 << 20, "100": 100, "50GiB": 50 << 30} {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("%s: %d %v", in, got, err)
		}
	}
	d, err := Draft(Request{Filename: "inventory.json", Data: []byte(`{"project":"shop","services":[{"name":"web","image":"app:1"}],"buckets":[{"name":"small","size":"50GiB"},{"name":"big","size":"51GiB"}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if d.Buckets[0].Target != TargetRustFS || d.Buckets[1].Target != TargetExternal {
		t.Fatalf("default targets %+v", d.Buckets)
	}
}
