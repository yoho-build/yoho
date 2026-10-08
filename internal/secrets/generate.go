package secrets

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// Generate returns a new random secret of the given kind:
// password32, password64 (URL-safe alphanumerics), hex32, hex64,
// base64_32, base64_64 (N random bytes, std base64).
func Generate(kind string) (string, error) {
	switch kind {
	case "password32":
		return password(32)
	case "password64":
		return password(64)
	case "hex32":
		return randHex(32)
	case "hex64":
		return randHex(64)
	case "base64_32":
		return randBase64(32)
	case "base64_64":
		return randBase64(64)
	}
	return "", fmt.Errorf("unknown generated secret kind %q (want password32, password64, hex32, hex64, base64_32, base64_64)", kind)
}

const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

func password(n int) (string, error) {
	out := make([]byte, n)
	buf := make([]byte, 1)
	for i := 0; i < n; {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		// Rejection sampling avoids modulo bias (62 * 4 = 248).
		if buf[0] >= 248 {
			continue
		}
		out[i] = alphabet[int(buf[0])%len(alphabet)]
		i++
	}
	return string(out), nil
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func randBase64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// Fingerprint is the audit fingerprint of a secret value: HMAC-SHA256 keyed
// per Destination, first 16 hex chars. Plain hashes are not used because
// low-entropy secrets could be brute-forced from them.
func Fingerprint(key []byte, value string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(value))
	return hex.EncodeToString(m.Sum(nil))[:16]
}
