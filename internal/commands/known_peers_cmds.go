package commands

import (
	"fmt"
	"strings"

	"github.com/bonjou-app/bonjou-cli/internal/network"
)

// cmdFingerprint shows the local public-key fingerprint (so the user can
// recite it to a peer over a side channel) and, optionally, the fingerprint
// of a specific discovered peer. Out-of-band fingerprint comparison is the
// only way to be sure TOFU pinned the right key on first observation.
func (h *Handler) cmdFingerprint(arg string) (Result, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		// Local fingerprint
		secret := h.session.Config.Secret
		if strings.TrimSpace(secret) == "" {
			return Result{Output: "Local fingerprint unavailable: no secret configured."}, nil
		}
		pubkey, err := network.LocalPublicKeyFromSecret(secret)
		if err != nil {
			return Result{Output: fmt.Sprintf("Failed to derive local public key: %v", err)}, nil
		}
		fp := network.Fingerprint(pubkey)
		var sb strings.Builder
		fmt.Fprintf(&sb, "Local username:  %s\n", h.session.Config.Username)
		fmt.Fprintf(&sb, "Local public key: %s\n", pubkey)
		fmt.Fprintf(&sb, "Fingerprint:      %s\n", fp)
		sb.WriteString("Share the fingerprint with the peer out-of-band so they can verify it on their @users list before sending anything sensitive.")
		return Result{Output: sb.String()}, nil
	}

	// Peer fingerprint by username or IP.
	peer, err := h.resolvePeer(arg)
	if err != nil || peer == nil {
		return asOutput(fmt.Sprintf("Peer '%s' not in current discovery list.", arg))
	}
	if strings.TrimSpace(peer.PublicKey) == "" {
		return Result{Output: fmt.Sprintf("No public key yet for '%s'. Wait for the next announcement.", arg)}, nil
	}
	fp := network.Fingerprint(peer.PublicKey)
	pinnedNote := ""
	if known := h.knownPeers(); known != nil {
		if entry := known.Find(peer.Username); entry != nil {
			if entry.PublicKey == peer.PublicKey {
				pinnedNote = "  (pinned ✓)"
			} else {
				pinnedNote = "  (MISMATCH — pinned fingerprint is " + entry.Fingerprint + ")"
			}
		} else {
			pinnedNote = "  (not yet pinned)"
		}
	}
	return Result{Output: fmt.Sprintf("%s @ %s\nFingerprint: %s%s", peer.Username, peer.IP, fp, pinnedNote)}, nil
}

// cmdTrust pins a peer's currently-advertised key, replacing any prior
// binding. Use after verifying the fingerprint out-of-band (e.g. the peer
// rotated devices and you've confirmed the new fingerprint over the phone).
func (h *Handler) cmdTrust(args string) (Result, error) {
	target := strings.TrimSpace(args)
	if target == "" {
		return Result{Output: "Usage: @trust <username|ip>"}, nil
	}
	known := h.knownPeers()
	if known == nil {
		return Result{Output: "Known-peers store is not available."}, nil
	}
	peer, err := h.resolvePeer(target)
	if err != nil || peer == nil {
		return asOutput(fmt.Sprintf("Peer '%s' is not in the discovery list. Wait for them to announce, then retry.", target))
	}
	if strings.TrimSpace(peer.PublicKey) == "" {
		return Result{Output: fmt.Sprintf("No public key yet for '%s'. Wait for the next announcement.", target)}, nil
	}
	if err := known.Trust(peer.Username, peer.PublicKey); err != nil {
		return Result{Output: fmt.Sprintf("Failed to pin '%s': %v", peer.Username, err)}, nil
	}
	return Result{Output: fmt.Sprintf("Pinned %s @ %s — fingerprint %s", peer.Username, peer.IP, network.Fingerprint(peer.PublicKey))}, nil
}

// cmdForget removes a pinned binding so the next announcement under that
// username is treated as a first observation. Use to recover from a peer
// rotating keys (e.g. reinstalled Bonjou or moved to a new device).
func (h *Handler) cmdForget(args string) (Result, error) {
	target := strings.TrimSpace(args)
	if target == "" {
		return Result{Output: "Usage: @forget <username>"}, nil
	}
	known := h.knownPeers()
	if known == nil {
		return Result{Output: "Known-peers store is not available."}, nil
	}
	if err := known.Forget(target); err != nil {
		return asOutput(fmt.Sprintf("No pinned key for '%s'.", target))
	}
	return Result{Output: fmt.Sprintf("Forgot pinned key for '%s'. The next announcement under that name will pin a new key.", target)}, nil
}

// cmdKnown lists every pinned binding so the user can audit who they're
// configured to accept messages from.
func (h *Handler) cmdKnown() (Result, error) {
	known := h.knownPeers()
	if known == nil {
		return Result{Output: "Known-peers store is not available."}, nil
	}
	entries := known.List()
	if len(entries) == 0 {
		return Result{Output: "No pinned peers yet. The first announcement from each peer will pin their key automatically."}, nil
	}
	var sb strings.Builder
	sb.WriteString("Pinned peers:\n")
	for _, entry := range entries {
		fmt.Fprintf(&sb, "  %-24s %s\n", entry.Username, entry.Fingerprint)
	}
	return Result{Output: strings.TrimRight(sb.String(), "\n")}, nil
}

func (h *Handler) knownPeers() *network.KnownPeers {
	if h == nil || h.session == nil || h.session.Discovery == nil {
		return nil
	}
	return h.session.Discovery.KnownPeers()
}
