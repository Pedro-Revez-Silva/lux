// Package ids mints the identifiers lux hands out: short, prefixed, random.
package ids

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Prefixes make an id's kind obvious in logs and URLs.
const (
	Tenant    = "ten"
	APIKey    = "key"
	HostToken = "htk"
	Host      = "host"
	Pool      = "pool"
	Run       = "run"
	Placement = "plc"
	Blob      = "blob"
	Snapshot  = "snap"
	Artifact  = "art"
	Server    = "srv"
)

// alphabet is lowercase base32 without padding: safe in URLs, container
// names, volume names and S3 keys.
const alphabet = "abcdefghijklmnopqrstuvwxyz234567"

func random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	var sb strings.Builder
	for _, c := range b {
		sb.WriteByte(alphabet[int(c)%len(alphabet)])
	}
	return sb.String()
}

// New returns "<prefix>_<16 random chars>".
func New(prefix string) string { return prefix + "_" + random(16) }

// Secret returns a bearer credential: "<prefix>_<40 random chars>".
func Secret(prefix string) string { return prefix + "_" + random(40) }

// Hash is how credentials are stored: never the value itself.
func Hash(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}
