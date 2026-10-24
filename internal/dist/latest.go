package dist

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const maxLatestResponse = 1 << 20

// Latest returns the latest published, stable Yoho release tag.
func Latest(ctx context.Context) (string, error) {
	endpoint := strings.TrimRight(APIBase, "/") + "/repos/" + Repo + "/releases/latest"
	body, err := getLimit(ctx, endpoint, currentToken(ctx), acceptAPI, maxLatestResponse)
	if err != nil {
		return "", fmt.Errorf("latest release: %w", err)
	}

	var release struct {
		TagName    *string `json:"tag_name"`
		Draft      *bool   `json:"draft"`
		Prerelease *bool   `json:"prerelease"`
	}
	if err := json.Unmarshal(body, &release); err != nil {
		return "", fmt.Errorf("latest release: invalid response: %w", err)
	}
	if release.TagName == nil || release.Draft == nil || release.Prerelease == nil {
		return "", fmt.Errorf("latest release: response is missing tag_name, draft, or prerelease")
	}
	if *release.Draft {
		return "", fmt.Errorf("latest release %q is a draft", *release.TagName)
	}
	if *release.Prerelease {
		return "", fmt.Errorf("latest release %q is a prerelease", *release.TagName)
	}
	if !isStableTag(*release.TagName) {
		return "", fmt.Errorf("latest release has invalid stable tag %q", *release.TagName)
	}
	return *release.TagName, nil
}

func isStableTag(tag string) bool {
	if !strings.HasPrefix(tag, "v") {
		return false
	}
	parts := strings.Split(tag[1:], ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return false
		}
		for i := 0; i < len(part); i++ {
			if part[i] < '0' || part[i] > '9' {
				return false
			}
		}
	}
	return true
}
