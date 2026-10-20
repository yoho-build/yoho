package secrets

import (
	"bytes"
	"regexp"
)

// accessTokenRe matches Yoho access tokens and device credentials
// (yoho_token_<random>_<checksum>, yoho_device_<random>_<checksum>) so they
// are masked even when they were never loaded as a secret value.
var accessTokenRe = regexp.MustCompile(`yoho_(token|device)_[A-Za-z0-9]{43}_[A-Za-z0-9]{6}`)

// Shape of a token, matching internal/accesstoken: 43 random base62
// characters, "_", then a 6 character checksum.
const (
	tokenRandomLen   = 43
	tokenChecksumLen = 6
)

var tokenPrefixes = [][]byte{[]byte("yoho_token_"), []byte("yoho_device_")}

// maxTokenLen is the longest possible token, which bounds the holdback.
var maxTokenLen = len("yoho_device_") + tokenRandomLen + 1 + tokenChecksumLen

// findAccessToken returns the leftmost token in data[i:] as absolute offsets,
// or -1, -1.
func findAccessToken(data []byte, i int) (start, end int) {
	loc := accessTokenRe.FindIndex(data[i:])
	if loc == nil {
		return -1, -1
	}
	return i + loc[0], i + loc[1]
}

// redactAccessTokens masks every Access Token in s.
func redactAccessTokens(s string) string {
	return accessTokenRe.ReplaceAllStringFunc(s, func(m string) string {
		return string(accessTokenLabel([]byte(m)))
	})
}

func accessTokenLabel(tok []byte) []byte {
	if bytes.HasPrefix(tok, []byte("yoho_device_")) {
		return []byte("[REDACTED:yoho_device]")
	}
	return []byte("[REDACTED:yoho_token]")
}

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// tokenPrefixViable reports whether b is a proper prefix of some token, i.e.
// later writes could still complete it into a match.
func tokenPrefixViable(b []byte) bool {
	for _, p := range tokenPrefixes {
		if len(b) <= len(p) {
			if bytes.HasPrefix(p, b) {
				return true
			}
			continue
		}
		if !bytes.HasPrefix(b, p) {
			continue
		}
		rest := b[len(p):]
		if len(rest) >= tokenRandomLen+1+tokenChecksumLen {
			continue // complete: a match, not a partial
		}
		ok := true
		for i, c := range rest {
			switch {
			case i == tokenRandomLen:
				ok = c == '_'
			default:
				ok = isAlnum(c)
			}
			if !ok {
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// accessTokenHold is the length of the suffix of data starting at the
// earliest position where a token could still be beginning. It is at most
// maxTokenLen-1.
func accessTokenHold(data []byte) int {
	for s := max(0, len(data)-(maxTokenLen-1)); s < len(data); s++ {
		if data[s] == 'y' && tokenPrefixViable(data[s:]) {
			return len(data) - s
		}
	}
	return 0
}
