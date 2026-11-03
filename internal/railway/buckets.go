package railway

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/yoho-build/yoho/internal/templates"
)

// bucket is one Railway bucket in the inventory: names and sizes, never
// credentials.
type bucket struct {
	Name   string `json:"name"`
	Size   string `json:"size"`
	Target string `json:"target,omitempty"`
}

// BucketPlan is where one bucket goes.
type BucketPlan struct {
	Name   string `json:"name"`
	Size   string `json:"size"`
	Bytes  int64  `json:"bytes"`
	Target string `json:"target"` // rustfs | external
}

// Bucket targets.
const (
	TargetRustFS   = "rustfs"
	TargetExternal = "external"
)

// rustfsMaxDefault is the size up to which a bucket without a target goes
// to the rustfs Service; larger ones go to an external store.
const rustfsMaxDefault = 50 << 30

// External store secret keys the App's Services list in x-yoho.secrets.
var externalKeys = []string{"S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY"}

var (
	bucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	sizePattern   = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([KMGT]i?B|B)?$`)
)

func prepareBuckets(inv *inventory) ([]BucketPlan, error) {
	if len(inv.Buckets) > 100 {
		return nil, &InputError{Message: "too many buckets", Hint: "draft one project at a time"}
	}
	seen := map[string]bool{}
	var out []BucketPlan
	for _, b := range inv.Buckets {
		if !bucketPattern.MatchString(b.Name) || strings.Contains(b.Name, "..") {
			return nil, &InputError{Message: "bucket name " + quote(b.Name) + " is not an S3 bucket name", Hint: "use 3-63 lowercase letters, digits, dots and hyphens, as the bucket is named in Railway"}
		}
		if seen[b.Name] {
			return nil, &InputError{Message: "duplicate bucket " + b.Name, Hint: "list each bucket once"}
		}
		seen[b.Name] = true
		n, err := parseSize(b.Size)
		if err != nil {
			return nil, &InputError{Message: "bucket " + b.Name + ": size " + quote(b.Size) + " is not a size", Hint: "use a string such as \"800MB\", \"40GB\" or \"1.5TB\" (from the bucket's usage in Railway)"}
		}
		target := strings.ToLower(strings.TrimSpace(b.Target))
		switch target {
		case "":
			target = TargetRustFS
			if n > rustfsMaxDefault {
				target = TargetExternal
			}
		case TargetRustFS, TargetExternal:
		default:
			return nil, &InputError{Message: "bucket " + b.Name + ": target " + quote(b.Target) + " is not rustfs or external", Hint: "rustfs runs object storage on the Server; external is R2, B2, S3 or another S3-compatible store"}
		}
		out = append(out, BucketPlan{Name: b.Name, Size: strings.TrimSpace(b.Size), Bytes: n, Target: target})
	}
	return out, nil
}

// parseSize reads "40GB", "1.5 TB", "512MiB" or a byte count. Decimal units
// are powers of 1000, binary ones (KiB...) powers of 1024.
func parseSize(s string) (int64, error) {
	m := sizePattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, errors.New("bad size")
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, err
	}
	mult := map[string]float64{"": 1, "B": 1,
		"KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12,
		"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40}[m[2]]
	v := f * mult
	if v > 1<<62 {
		return 0, errors.New("too large")
	}
	return int64(v), nil
}

// renderBuckets adds the rustfs Service when a bucket goes there, and the
// migration checklist. It runs after the compose text exists.
func renderBuckets(d *Result, inv inventory) error {
	var rustfs, external []BucketPlan
	for _, b := range d.Buckets {
		if b.Target == TargetRustFS {
			rustfs = append(rustfs, b)
		} else {
			external = append(external, b)
		}
	}
	if len(d.Buckets) == 0 {
		return nil
	}
	if len(rustfs) > 0 {
		m, err := templates.Merge([]byte(d.Compose), []string{"rustfs"})
		if err != nil {
			var te *templates.Error
			if errors.As(err, &te) {
				return &InputError{Message: "add the rustfs Service: " + te.Message, Hint: "rename the inventory Service or volume called rustfs/rustfsdata, or set the buckets' target to external"}
			}
			return err
		}
		d.Compose = string(m.Compose)
		d.Services = append(d.Services, m.Services...)
		d.Volumes = append(d.Volumes, m.Volumes...)
		t, _ := templates.Lookup("rustfs")
		for _, k := range t.Secrets {
			if contains(d.Generated, k) {
				return &InputError{Message: k + " is generated twice", Hint: "remove the other generator of " + k}
			}
			d.Generated = append(d.Generated, k)
		}
	}
	project := "yoho-" + inv.Project + "-production"
	gen := "/var/lib/yoho/apps/" + inv.Project + "/production/generated/"
	op := func(k string) string { return "$(op read op://Production/" + inv.Project + "/" + k + ")" }
	c := []string{
		"# Copy each bucket with rclone. Credentials come from the environment, never argv.",
		"# Railway side: the bucket's S3 endpoint and keys, stored in your password manager.",
		"export RCLONE_CONFIG_RAILWAY_TYPE=s3 RCLONE_CONFIG_RAILWAY_PROVIDER=Other",
		`export RCLONE_CONFIG_RAILWAY_ENDPOINT="` + op("RAILWAY_BUCKET_ENDPOINT") + `"`,
		`export RCLONE_CONFIG_RAILWAY_ACCESS_KEY_ID="` + op("RAILWAY_BUCKET_ACCESS_KEY_ID") + `"`,
		`export RCLONE_CONFIG_RAILWAY_SECRET_ACCESS_KEY="` + op("RAILWAY_BUCKET_SECRET_ACCESS_KEY") + `"`,
	}
	if len(rustfs) > 0 {
		c = append(c,
			"# RustFS on the Server, after the first `yoho deploy`: tunnel its S3 port to 127.0.0.1:19000.",
			`ssh root@YOUR_SERVER "docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' `+project+`-rustfs-1"`,
			"ssh -N -L 19000:RUSTFS_IP:9000 root@YOUR_SERVER &",
			"export RCLONE_CONFIG_YOHO_TYPE=s3 RCLONE_CONFIG_YOHO_PROVIDER=Other RCLONE_CONFIG_YOHO_ENDPOINT=http://127.0.0.1:19000",
			`export RCLONE_CONFIG_YOHO_ACCESS_KEY_ID="$(ssh root@YOUR_SERVER cat `+gen+`RUSTFS_ACCESS_KEY)"`,
			`export RCLONE_CONFIG_YOHO_SECRET_ACCESS_KEY="$(ssh root@YOUR_SERVER cat `+gen+`RUSTFS_SECRET_KEY)"`,
		)
		for _, b := range rustfs {
			c = append(c,
				"# "+b.Name+" ("+b.Size+") -> rustfs",
				"rclone mkdir yoho:"+b.Name,
				"rclone sync railway:"+b.Name+" yoho:"+b.Name+" --checksum --progress",
				"rclone check railway:"+b.Name+" yoho:"+b.Name,
			)
		}
	}
	if len(external) > 0 {
		c = append(c,
			"# External store (R2 shown; PROVIDER=AWS for S3, Other for B2). Create the buckets there first.",
			"export RCLONE_CONFIG_TARGET_TYPE=s3 RCLONE_CONFIG_TARGET_PROVIDER=Cloudflare RCLONE_CONFIG_TARGET_ENDPOINT=https://ACCOUNT_ID.r2.cloudflarestorage.com",
			`export RCLONE_CONFIG_TARGET_ACCESS_KEY_ID="`+op(externalKeys[0])+`"`,
			`export RCLONE_CONFIG_TARGET_SECRET_ACCESS_KEY="`+op(externalKeys[1])+`"`,
		)
		for _, b := range external {
			c = append(c,
				"# "+b.Name+" ("+b.Size+") -> external",
				"rclone sync railway:"+b.Name+" target:"+b.Name+" --checksum --progress",
				"rclone check railway:"+b.Name+" target:"+b.Name,
			)
		}
	}
	c = append(c, "# rclone check must report 0 differences. Objects without an MD5 (multipart uploads) are compared by size; add --download for a byte-for-byte check.")
	d.Checklist = c
	return nil
}

// bucketNotes are the yoho.yml comment lines that tell the App's Services
// how to reach each bucket.
func bucketNotes(d *Result) []string {
	var rustfs, external []string
	for _, b := range d.Buckets {
		if b.Target == TargetRustFS {
			rustfs = append(rustfs, b.Name)
		} else {
			external = append(external, b.Name)
		}
	}
	subject := func(names []string) string {
		if len(names) == 1 {
			return "Bucket " + names[0] + " goes"
		}
		return "Buckets " + strings.Join(names, ", ") + " go"
	}
	var out []string
	if len(rustfs) > 0 {
		out = append(out,
			subject(rustfs)+" to the rustfs Service (S3 endpoint http://rustfs:9000, path-style).",
			"  A Service that uses it lists RUSTFS_ACCESS_KEY and RUSTFS_SECRET_KEY in x-yoho.secrets (generated on the Server).",
		)
	}
	if len(external) > 0 {
		out = append(out,
			subject(external)+" to an external S3-compatible store (R2, B2, S3).",
			"  A Service that uses it sets environment S3_ENDPOINT (https://ACCOUNT_ID.r2.cloudflarestorage.com)",
			"  and lists "+strings.Join(externalKeys, " and ")+" in x-yoho.secrets.",
		)
	}
	return out
}
