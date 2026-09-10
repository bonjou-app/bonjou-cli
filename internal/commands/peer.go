package commands

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/bonjou-app/bonjou-cli/internal/network"
)

// resolvePeer turns a user-supplied target (username, IP, hostname) into a
// *network.Peer pulled from the discovery cache. If the input parses as an
// IP we resolve by IP first; otherwise we fall back to a username lookup.
func (h *Handler) resolvePeer(target string) (*network.Peer, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, errors.New("empty target")
	}
	if ip := net.ParseIP(target); ip != nil {
		peer, err := h.session.Discovery.Resolve(ip.String())
		if err != nil {
			return nil, fmt.Errorf("peer %s not discovered", target)
		}
		return peer, nil
	}
	peer, err := h.session.Discovery.Resolve(target)
	if err != nil {
		return nil, err
	}
	return peer, nil
}

func peerLabel(peer *network.Peer) string {
	if peer.Username != "" {
		return fmt.Sprintf("%s@%s:%d", peer.Username, peer.IP, peer.Port)
	}
	return fmt.Sprintf("%s:%d", peer.IP, peer.Port)
}

func safePeerLabel(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "(unknown)"
	}
	return trimmed
}

func seenLabel(lastSeen time.Time) string {
	if lastSeen.IsZero() {
		return "seen recently"
	}
	diff := time.Since(lastSeen)
	if diff < 0 {
		diff = 0
	}
	switch {
	case diff < 1500*time.Millisecond:
		return "seen just now"
	case diff < time.Minute:
		secs := int(diff.Round(time.Second) / time.Second)
		if secs <= 1 {
			return "seen 1s ago"
		}
		return fmt.Sprintf("seen %ds ago", secs)
	case diff < time.Hour:
		mins := int(diff.Round(time.Minute) / time.Minute)
		if mins <= 1 {
			return "seen 1m ago"
		}
		return fmt.Sprintf("seen %dm ago", mins)
	case diff < 24*time.Hour:
		hours := int(diff.Round(time.Hour) / time.Hour)
		if hours <= 1 {
			return "seen 1h ago"
		}
		return fmt.Sprintf("seen %dh ago", hours)
	default:
		days := int(diff.Round(24*time.Hour) / (24 * time.Hour))
		if days <= 1 {
			return "seen 1d ago"
		}
		return fmt.Sprintf("seen %dd ago", days)
	}
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit <= 3 {
		return string(runes[:limit])
	}
	return string(runes[:limit-3]) + "..."
}

// sanitiseUsername normalises a user-supplied name to a safe, printable
// form. It collapses internal whitespace to '-' and drops control,
// surrogate, zero-width, and bidi-override characters so a peer cannot
// inject log-spoofing payloads via their announced name. The second
// return value reports whether any character had to be removed or
// translated, so the caller can tell the user what happened.
func sanitiseUsername(input string) (string, bool) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", false
	}
	// Drop anything that isn't safe to render directly in a terminal.
	var b strings.Builder
	removed := false
	for _, r := range trimmed {
		if isUnsafeRune(r) {
			removed = true
			continue
		}
		b.WriteRune(r)
	}
	cleaned := strings.TrimSpace(b.String())
	if cleaned == "" {
		return "", false
	}
	parts := strings.Fields(cleaned)
	if len(parts) == 0 {
		return "", false
	}
	joined := strings.Join(parts, "-")
	return joined, removed || joined != trimmed
}

// isUnsafeRune reports whether r should be stripped from a username
// before it is displayed, logged, or broadcast. Beyond the standard
// IsControl set, we also reject:
//   - zero-width characters (U+200B..U+200F, U+FEFF), which can hide
//     differences between visually identical names;
//   - bidirectional overrides (U+202A..U+202E, U+2066..U+2069), which
//     can flip the displayed direction of subsequent text;
//   - non-printable / Cs (surrogate) runes.
func isUnsafeRune(r rune) bool {
	if r == '\t' || r == ' ' {
		return false // collapsed into '-' by sanitiseUsername
	}
	// Zero-width and bidi-override codepoints by hex literal so the
	// source file stays well-formed (some of them are otherwise rejected
	// at parse time as invalid rune literals or BOMs).
	switch r {
	case 0x200B, 0x200C, 0x200D, 0x200E, 0x200F, 0xFEFF: // zero-width joiners
		return true
	case 0x202A, 0x202B, 0x202C, 0x202D, 0x202E: // bidi formatting
		return true
	case 0x2066, 0x2067, 0x2068, 0x2069: // bidi isolates
		return true
	}
	if r < 0x20 || r == 0x7f {
		return true
	}
	if r >= 0xD800 && r <= 0xDFFF {
		return true
	}
	return false
}

// resolveUniqueUsername picks a non-colliding name. If `base` is already in
// use by a discovered peer, we append a deterministic suffix derived from
// the local IP, falling back to numeric variants until something unique is
// found.
func resolveUniqueUsername(base, localIP string, peers []network.Peer) (string, bool) {
	if !usernameTaken(base, peers) {
		return base, false
	}
	suffix := usernameCollisionSuffix(localIP)
	candidate := appendUsernameSuffix(base, suffix)
	if !usernameTaken(candidate, peers) {
		return candidate, true
	}
	for i := 2; ; i++ {
		candidate = appendUsernameSuffix(base, fmt.Sprintf("%s-%d", suffix, i))
		if !usernameTaken(candidate, peers) {
			return candidate, true
		}
	}
}

func usernameTaken(candidate string, peers []network.Peer) bool {
	for _, peer := range peers {
		if strings.EqualFold(peer.Username, candidate) {
			return true
		}
	}
	return false
}

func usernameCollisionSuffix(localIP string) string {
	ip := net.ParseIP(strings.TrimSpace(localIP))
	if ip4 := ip.To4(); ip4 != nil {
		return fmt.Sprintf("%d-%d", ip4[2], ip4[3])
	}
	if ip != nil {
		raw := ip.String()
		if raw != "" {
			raw = strings.ReplaceAll(raw, ":", "-")
			raw = strings.Trim(raw, "-")
			if raw != "" {
				if len(raw) > 12 {
					raw = raw[len(raw)-12:]
				}
				return raw
			}
		}
	}
	return "peer"
}

func appendUsernameSuffix(base, suffix string) string {
	const maxUsernameLen = 64
	base = strings.TrimSpace(base)
	suffix = strings.Trim(strings.TrimSpace(suffix), "-")
	if base == "" {
		base = "bonjou-user"
	}
	if suffix == "" {
		suffix = "peer"
	}
	available := maxUsernameLen - len(suffix) - 1
	if available < 1 {
		available = 1
	}
	if len(base) > available {
		base = base[:available]
	}
	return base + "-" + suffix
}
