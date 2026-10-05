package relay

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
)

// codeAlphabet omits every vowel — including Y — so a generated code can
// never spell a word, and omits the glyph pairs 0/O and 1/I/L so a code
// survives being read aloud or copied by hand. 27 symbols over 6 positions
// gives ~387M combinations, which with per-IP rate limiting and a short
// room TTL puts enumeration out of reach.
const codeAlphabet = "23456789BCDFGHJKMNPQRSTVWXZ"

const (
	codeGroupLen   = 3
	codeGroupCount = 2
	codeLen        = codeGroupLen * codeGroupCount
)

// newRoomCode returns a room code formatted as two dash-separated groups,
// e.g. "7K2-9QX".
func newRoomCode() (string, error) {
	limit := big.NewInt(int64(len(codeAlphabet)))
	var b strings.Builder
	b.Grow(codeLen + codeGroupCount - 1)
	for i := 0; i < codeLen; i++ {
		if i > 0 && i%codeGroupLen == 0 {
			b.WriteByte('-')
		}
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", fmt.Errorf("generate room code: %w", err)
		}
		b.WriteByte(codeAlphabet[n.Int64()])
	}
	return b.String(), nil
}

// normalizeCode canonicalises user input so "7k2 9qx" and "7K2-9QX" reach
// the same room. Characters outside the alphabet are dropped rather than
// corrected: every excluded glyph was excluded precisely because it is
// ambiguous, so guessing the user's intent would be a coin flip.
func normalizeCode(raw string) string {
	upper := strings.ToUpper(strings.TrimSpace(raw))
	var b strings.Builder
	b.Grow(codeLen)
	for _, r := range upper {
		if strings.ContainsRune(codeAlphabet, r) {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) != codeLen {
		return ""
	}
	return formatCode(out)
}

// formatCode inserts group separators into a bare code.
func formatCode(bare string) string {
	if len(bare) != codeLen {
		return bare
	}
	var b strings.Builder
	b.Grow(codeLen + codeGroupCount - 1)
	for i := 0; i < codeLen; i++ {
		if i > 0 && i%codeGroupLen == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(bare[i])
	}
	return b.String()
}

// newID returns a 16-hex-character identifier for a peer.
func newID() (string, error) {
	return randomHex(8)
}

func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate random id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
