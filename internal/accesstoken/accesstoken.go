// Package accesstoken generates, validates and hashes Yoho access tokens and
// device credentials (ADR 0011). A plaintext value is shown once and stored
// only as a hash. Format: prefix, 43 base62 characters encoding 32 random
// bytes, "_", then a 6-character base62 CRC32 of everything before the "_".
package accesstoken

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"math/big"
	"strings"
)

// Kind distinguishes access tokens from device credentials.
type Kind int

const (
	Token Kind = iota + 1
	Device
)

const (
	tokenPrefix  = "yoho_token_"
	devicePrefix = "yoho_device_"

	alphabet    = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	randomBytes = 32
	randomLen   = 43 // base62 digits needed for 256 bits
	checksumLen = 6
	shortLen    = 4

	// HashPrefix tags stored hashes with their algorithm.
	HashPrefix = "sha256:"
)

var (
	// ErrMalformed: not shaped like a Yoho token.
	ErrMalformed = errors.New("not a Yoho access token")
	// ErrChecksum: shaped like a token but the checksum fails, i.e. a typo.
	ErrChecksum = errors.New("access token checksum mismatch: check for a typo or truncated paste")
)

// Prefix is the literal prefix for k.
func (k Kind) Prefix() string {
	switch k {
	case Token:
		return tokenPrefix
	case Device:
		return devicePrefix
	}
	return ""
}

func (k Kind) String() string {
	switch k {
	case Token:
		return "token"
	case Device:
		return "device"
	}
	return "unknown"
}

// New returns a fresh plaintext value of the given kind.
func New(kind Kind) (string, error) {
	prefix := kind.Prefix()
	if prefix == "" {
		return "", fmt.Errorf("accesstoken: unknown kind %d", int(kind))
	}
	buf := make([]byte, randomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("accesstoken: random: %w", err)
	}
	body := prefix + encode62(new(big.Int).SetBytes(buf), randomLen)
	return body + "_" + checksum(body), nil
}

// Parse validates s and reports its Kind. It returns ErrChecksum for a value
// that is otherwise well formed, so callers can say "typo" rather than
// "unknown token".
func Parse(s string) (Kind, error) {
	var kind Kind
	var rest string
	switch {
	case strings.HasPrefix(s, tokenPrefix):
		kind, rest = Token, s[len(tokenPrefix):]
	case strings.HasPrefix(s, devicePrefix):
		kind, rest = Device, s[len(devicePrefix):]
	default:
		return 0, fmt.Errorf("%w: unknown prefix", ErrMalformed)
	}
	if len(rest) != randomLen+1+checksumLen {
		return 0, fmt.Errorf("%w: wrong length", ErrMalformed)
	}
	random, sep, sum := rest[:randomLen], rest[randomLen], rest[randomLen+1:]
	if sep != '_' {
		return 0, fmt.Errorf("%w: missing checksum separator", ErrMalformed)
	}
	if !isBase62(random) || !isBase62(sum) {
		return 0, fmt.Errorf("%w: invalid characters", ErrMalformed)
	}
	if subtle.ConstantTimeCompare([]byte(sum), []byte(checksum(kind.Prefix()+random))) != 1 {
		return 0, ErrChecksum
	}
	return kind, nil
}

// Hash returns the storable form of a plaintext value: "sha256:<hex>".
func Hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return HashPrefix + hex.EncodeToString(sum[:])
}

// ConstantTimeMatch reports whether plaintext hashes to hash.
func ConstantTimeMatch(plaintext, hash string) bool {
	return subtle.ConstantTimeCompare([]byte(Hash(plaintext)), []byte(hash)) == 1
}

// InvalidShort is what Short prints for a token-shaped value whose
// checksum does not match.
const InvalidShort = "yoho_…invalid"

// Short is a display form safe to print: the prefix plus the first four
// characters, such as "yoho_token_4fK9…". Values that are not token-shaped
// yield "…" only, and a token-shaped value with a bad checksum (a typo,
// or a real token with one character changed) yields the fixed
// InvalidShort, so arbitrary input is never echoed.
func Short(s string) string {
	kind, err := Parse(s)
	if errors.Is(err, ErrChecksum) {
		return InvalidShort
	}
	if err != nil {
		return "…"
	}
	p := kind.Prefix()
	return p + s[len(p):len(p)+shortLen] + "…"
}

func checksum(s string) string {
	return encode62(big.NewInt(int64(crc32.ChecksumIEEE([]byte(s)))), checksumLen)
}

// encode62 renders n in base62, left-padded with '0' to width.
func encode62(n *big.Int, width int) string {
	out := make([]byte, width)
	for i := range out {
		out[i] = alphabet[0]
	}
	base, mod := big.NewInt(62), new(big.Int)
	for i := width - 1; i >= 0 && n.Sign() > 0; i-- {
		n.DivMod(n, base, mod)
		out[i] = alphabet[mod.Int64()]
	}
	return string(out)
}

func isBase62(s string) bool {
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune(alphabet, rune(s[i])) {
			return false
		}
	}
	return true
}
