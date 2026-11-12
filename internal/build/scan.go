package build

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/yoho-build/yoho/internal/ui"
)

var lookPath = exec.LookPath

type scanCounts struct{ Critical, High int }

func parseTrivy(data []byte) (scanCounts, error) {
	var report struct {
		Results []struct{ Vulnerabilities []struct{ Severity string } }
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return scanCounts{}, err
	}
	var c scanCounts
	for _, result := range report.Results {
		for _, v := range result.Vulnerabilities {
			c.add(v.Severity)
		}
	}
	return c, nil
}

func parseGrype(data []byte) (scanCounts, error) {
	var report struct {
		Matches []struct{ Vulnerability struct{ Severity string } }
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return scanCounts{}, err
	}
	var c scanCounts
	for _, m := range report.Matches {
		c.add(m.Vulnerability.Severity)
	}
	return c, nil
}

func (c *scanCounts) add(severity string) {
	switch strings.ToUpper(severity) {
	case "CRITICAL":
		c.Critical++
	case "HIGH":
		c.High++
	}
}

// ScanImages reports fixed High/Critical vulnerabilities in locally built images.
// Scanner failures and findings are warnings and never prevent rollout. run is
// injectable; nil uses os/exec. Scanner output is parsed, never printed.
func ScanImages(ctx context.Context, refs []string, engine, platform string, u *ui.UI, run Exec) {
	scanner := "trivy"
	bin, err := lookPath(scanner)
	if err != nil {
		scanner = "grype"
		bin, err = lookPath(scanner)
	}
	if err != nil {
		u.Warn("scan skipped: install trivy or grype")
		return
	}
	if run == nil {
		run = osExec
	}
	for _, ref := range refs {
		counts, err := scanImage(ctx, ref, engine, platform, scanner, bin, run)
		fields := map[string]any{"image": ref, "scanner": scanner, "critical": counts.Critical, "high": counts.High}
		if err != nil {
			fields["ok"] = false
			u.Warn("scan failed for %s (%s); run the scanner manually, or run `yoho deploy --refresh` to refresh base images", ref, scanner)
		} else {
			fields["ok"] = true
			if counts.Critical+counts.High > 0 {
				u.Warn("%s: %d critical, %d high vulnerabilities (%s); run `yoho deploy --refresh`", ref, counts.Critical, counts.High, scanner)
			} else {
				u.Info("%s: 0 critical, 0 high vulnerabilities (%s)", ref, scanner)
			}
		}
		u.Event("image.scan", "", fields)
	}
}

func scanImage(ctx context.Context, ref, engine, platform, scanner, bin string, run Exec) (scanCounts, error) {
	target := "docker:" + ref
	input := ""
	if engine == "container" {
		dir, err := os.MkdirTemp("", "yoho-scan-")
		if err != nil {
			return scanCounts{}, err
		}
		defer os.RemoveAll(dir)
		input = filepath.Join(dir, "image.tar")
		if err := run(ctx, Command{Argv: []string{"container", "image", "save", "--platform", platform, "--output", input, ref}}); err != nil {
			return scanCounts{}, err
		}
		target = "oci-archive:" + input
	}
	argv := []string{bin, target, "--only-fixed", "-o", "json"}
	parse := parseGrype
	if scanner == "trivy" {
		argv = []string{bin, "image", "--quiet", "--ignore-unfixed", "--severity", "HIGH,CRITICAL", "--format", "json"}
		if input != "" {
			argv = append(argv, "--input", input)
		} else {
			argv = append(argv, "--image-src", "docker", ref)
		}
		parse = parseTrivy
	}
	if platform != "" {
		argv = append(argv, "--platform", platform)
	}
	var out bytes.Buffer
	if err := run(ctx, Command{Argv: argv, Stdout: &out}); err != nil {
		return scanCounts{}, fmt.Errorf("scan: %w", err)
	}
	return parse(out.Bytes())
}
