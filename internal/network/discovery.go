package network

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/bonjou-app/bonjou-cli/internal/config"
	"github.com/bonjou-app/bonjou-cli/internal/logger"
)

// Peer represents an active Bonjou device on the LAN.
type Peer struct {
	Username  string
	IP        string
	Port      int
	LastSeen  time.Time
	PublicKey string
}

type announcement struct {
	Username  string `json:"username"`
	IP        string `json:"ip"`
	Port      int    `json:"port"`
	Timestamp int64  `json:"ts"`
	PublicKey string `json:"public_key,omitempty"`
}

// DiscoveryService handles LAN peer discovery over UDP broadcasts.
type DiscoveryService struct {
	cfg       *config.Config
	logger    *logger.Logger
	known     *KnownPeers
	peers     map[string]*Peer
	mu        sync.RWMutex
	localMu   sync.RWMutex
	stop      chan struct{}
	stopOnce  sync.Once
	wait      sync.WaitGroup
	started   bool
	localUser string
	localIP   string
	localPort int
}

func (d *DiscoveryService) isStopping() bool {
	select {
	case <-d.stop:
		return true
	default:
		return false
	}
}

func NewDiscoveryService(cfg *config.Config, logger *logger.Logger) *DiscoveryService {
	return &DiscoveryService{
		cfg:    cfg,
		logger: logger,
		peers:  make(map[string]*Peer),
		stop:   make(chan struct{}),
	}
}

// SetKnownPeers attaches a TOFU store to the discovery service. Once set,
// inbound announcements whose claimed public key does not match the pinned
// key for that username are dropped (impersonation defence).
func (d *DiscoveryService) SetKnownPeers(kp *KnownPeers) {
	d.known = kp
}

// KnownPeers returns the attached TOFU store (may be nil).
func (d *DiscoveryService) KnownPeers() *KnownPeers {
	return d.known
}

// Start launches announcer and listener goroutines.
func (d *DiscoveryService) Start(username, ip string, port int) error {
	if d.started {
		return nil
	}
	d.localMu.Lock()
	d.localUser = username
	d.localIP = ip
	d.localPort = port
	d.localMu.Unlock()
	d.wait.Add(2)
	go d.listenLoop()
	go d.announceLoop()
	d.started = true
	return nil
}

// Stop requests goroutines to wind down.
func (d *DiscoveryService) Stop() {
	if !d.started {
		return
	}
	d.stopOnce.Do(func() { close(d.stop) })
	d.wait.Wait()
	d.started = false
}

// UpdateLocalUser switches the announcer to a new username.
func (d *DiscoveryService) UpdateLocalUser(username string) {
	d.localMu.Lock()
	d.localUser = username
	d.localMu.Unlock()
}

// UpdateLocalEndpoint refreshes the local IP/port and resets peer cache for new networks.
func (d *DiscoveryService) UpdateLocalEndpoint(ip string, port int) {
	if ip == "" && port <= 0 {
		return
	}
	d.localMu.Lock()
	if ip != "" {
		d.localIP = ip
	}
	if port > 0 {
		d.localPort = port
	}
	d.localMu.Unlock()

	d.mu.Lock()
	d.peers = make(map[string]*Peer)
	d.mu.Unlock()

	go d.ForceAnnounce()
}

// ForceAnnounce immediately broadcasts the latest local identity.
func (d *DiscoveryService) ForceAnnounce() {
	if !d.started || d.isStopping() {
		return
	}
	payload, ok := d.prepareAnnouncement()
	if !ok {
		return
	}
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		d.logger.Error("force announce socket: %v", err)
		return
	}
	defer func() { _ = conn.Close() }()
	enableBroadcast(conn)
	_ = conn.SetWriteBuffer(1024)
	addrs := d.broadcastAddrs()
	d.writeAnnouncement(conn, payload, addrs)
}

// ListPeers returns peers observed within the freshness window.
func (d *DiscoveryService) ListPeers() []Peer {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []Peer
	expiry := time.Now().Add(-2 * time.Minute)
	for key, peer := range d.peers {
		if peer.LastSeen.Before(expiry) {
			delete(d.peers, key)
			continue
		}
		clone := *peer
		clone.PublicKey = ""
		out = append(out, clone)
	}
	return out
}

// Resolve takes a username or IP string and returns the matching peer.
func (d *DiscoveryService) Resolve(target string) (*Peer, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	// direct IP match first
	if peer, ok := d.peers[target]; ok {
		clone := *peer
		return &clone, nil
	}
	// Search by username (case-insensitive — the rest of the codebase uses
	// EqualFold for the same comparison, and not aligning here meant
	// "Alice" sometimes resolved and sometimes didn't).
	var matches []*Peer
	for _, peer := range d.peers {
		if strings.EqualFold(peer.Username, target) {
			matches = append(matches, peer)
		}
	}

	// Handle ambiguity: multiple peers with same username
	if len(matches) > 1 {
		var ips []string
		for _, peer := range matches {
			ips = append(ips, fmt.Sprintf("%s (offline since: %v)", peer.IP, time.Since(peer.LastSeen)))
		}
		return nil, fmt.Errorf("multiple peers named '%s' found: %v. Use IP address instead, e.g., @send %s message", target, ips, matches[0].IP)
	}

	if len(matches) == 1 {
		clone := *matches[0]
		return &clone, nil
	}

	return nil, errors.New("peer not found")
}

func sameSubnetIPv4(senderIP, localIP string) bool {
	sender := net.ParseIP(senderIP).To4()
	local := net.ParseIP(localIP).To4()
	if sender == nil || local == nil {
		return false
	}
	addrs, err := net.InterfaceAddrs()
	if err == nil {
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if ipNet.IP == nil || ipNet.IP.To4() == nil {
				continue
			}
			if ipNet.Contains(local) {
				return ipNet.Contains(sender)
			}
		}
	}
	// Conservative fallback if we can't identify the interface/netmask.
	return sender[0] == local[0] && sender[1] == local[1] && sender[2] == local[2]
}

// SharedPublicKey returns the most recent public key advertised by a peer.
func (d *DiscoveryService) SharedPublicKey(username, ip string) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if ip != "" {
		if peer, exists := d.peers[ip]; exists {
			if peer.PublicKey != "" {
				return peer.PublicKey, true
			}
		}
	}
	if username != "" {
		for _, peer := range d.peers {
			if peer.Username == username && peer.PublicKey != "" {
				return peer.PublicKey, true
			}
		}
	}
	return "", false
}

func (d *DiscoveryService) listenLoop() {
	defer d.wait.Done()
	addr := &net.UDPAddr{IP: net.IPv4zero, Port: d.cfg.DiscoveryPort}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		// Provide helpful error message for port conflicts
		if strings.Contains(err.Error(), "address already in use") {
			d.logger.Error("discovery port %d already in use - another Bonjou instance or application may be running", d.cfg.DiscoveryPort)
		} else if strings.Contains(err.Error(), "permission denied") {
			d.logger.Error("permission denied to listen on UDP port %d - you may need to use a port >1024 or run with elevated privileges", d.cfg.DiscoveryPort)
		} else {
			d.logger.Error("discovery listener failed: %v", err)
		}
		return
	}
	defer func() { _ = conn.Close() }()
	buf := make([]byte, 1024)
	for {
		if d.isStopping() {
			return
		}
		if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			d.logger.Error("discovery deadline error: %v", err)
			continue
		}
		n, remote, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				select {
				case <-d.stop:
					return
				default:
				}
				continue
			}
			d.logger.Error("discovery read error: %v", err)
			continue
		}
		var ann announcement
		if err := json.Unmarshal(buf[:n], &ann); err != nil {
			d.logger.Error("invalid announcement from %s: %v", remote.IP.String(), err)
			continue
		}

		senderIP := remote.IP.String()
		if senderIP == "" {
			senderIP = ann.IP
		}
		d.localMu.RLock()
		localIP := d.localIP
		localPort := d.localPort
		d.localMu.RUnlock()
		if senderIP == localIP && ann.Port == localPort {
			continue
		}
		// Enforce same-subnet operation: ignore announcements not from our subnet.
		if !sameSubnetIPv4(senderIP, localIP) {
			continue
		}

		// Drop announcements that advertise a port we don't speak. Bonjou
		// uses a single well-known TCP listen port; allowing arbitrary ports
		// would let an attacker advertise unrelated services (SSH, HTTP, …)
		// so other clients try to open a Bonjou session against them.
		expectedPort := d.cfg.ListenPort
		if expectedPort > 0 && ann.Port != expectedPort {
			d.logger.Error("discovery: dropping announcement from %s — port %d != expected %d", senderIP, ann.Port, expectedPort)
			continue
		}

		// TOFU: pin the username→pubkey binding on first sight and reject
		// subsequent announcements that claim the same username with a
		// different key. Without this, any LAN host can spoof a peer's
		// identity by broadcasting under their name with attacker-controlled
		// crypto material, then sit in the middle of all "encrypted" chat.
		username := strings.TrimSpace(ann.Username)
		if d.known != nil && username != "" && strings.TrimSpace(ann.PublicKey) != "" {
			outcome, pinned, err := d.known.Pin(username, ann.PublicKey)
			if err != nil {
				d.logger.Error("discovery: failed to update known peers: %v", err)
			}
			if outcome == PinMismatch {
				expectedFp := ""
				if pinned != nil {
					expectedFp = pinned.Fingerprint
				}
				d.logger.Error(
					"discovery: dropping announcement from %s — '%s' announced fingerprint %s but is pinned to %s. Use @forget %s to accept the new key.",
					senderIP, username, Fingerprint(ann.PublicKey), expectedFp, username,
				)
				continue
			}
		}

		// IMPORTANT: Trust the packet source IP for routing/reachability.
		// Payload IP can be wrong if the sender picked the wrong interface (VPN/Docker/etc.).
		peer := &Peer{Username: ann.Username, IP: senderIP, Port: ann.Port, LastSeen: time.Now(), PublicKey: ann.PublicKey}
		d.mu.Lock()
		d.peers[peer.IP] = peer
		d.mu.Unlock()
	}
}

func (d *DiscoveryService) announceLoop() {
	defer d.wait.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		d.logger.Error("discovery announcer failed: %v", err)
		return
	}
	defer func() { _ = conn.Close() }()
	enableBroadcast(conn)
	if err := conn.SetWriteBuffer(1024); err != nil {
		d.logger.Error("discovery write buffer: %v", err)
	}
	d.sendCurrentAnnouncement(conn)
	for {
		select {
		case <-ticker.C:
			d.sendCurrentAnnouncement(conn)
		case <-d.stop:
			return
		}
	}
}

func (d *DiscoveryService) sendCurrentAnnouncement(conn *net.UDPConn) {
	payload, ok := d.prepareAnnouncement()
	if !ok {
		return
	}
	addrs := d.broadcastAddrs()
	d.writeAnnouncement(conn, payload, addrs)
}

func (d *DiscoveryService) prepareAnnouncement() ([]byte, bool) {
	pubKey, err := localPublicKeyFromSecret(d.cfg.Secret)
	if err != nil {
		d.logger.Error("derive discovery public key: %v", err)
		return nil, false
	}
	d.localMu.RLock()
	ann := announcement{Username: d.localUser, IP: d.localIP, Port: d.localPort, Timestamp: time.Now().Unix(), PublicKey: pubKey}
	d.localMu.RUnlock()
	if ann.IP == "" || ann.Port == 0 {
		return nil, false
	}
	data, err := json.Marshal(ann)
	if err != nil {
		d.logger.Error("marshal announcement: %v", err)
		return nil, false
	}
	return data, true
}

func (d *DiscoveryService) broadcastAddrs() []*net.UDPAddr {
	seen := make(map[string]struct{})
	var addrs []*net.UDPAddr
	global := &net.UDPAddr{IP: net.IPv4bcast, Port: d.cfg.DiscoveryPort}
	addrs = append(addrs, global)
	seen[global.String()] = struct{}{}
	ifaces, err := net.Interfaces()
	if err != nil {
		d.logger.Error("list interfaces: %v", err)
		return addrs
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagBroadcast == 0 {
			continue
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addresses {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipNet.IP.To4()
			if ip == nil || ip.Equal(net.IPv4zero) {
				continue
			}
			mask := ipNet.Mask
			if len(mask) != net.IPv4len {
				continue
			}
			broadcast := net.IPv4(ip[0]|^mask[0], ip[1]|^mask[1], ip[2]|^mask[2], ip[3]|^mask[3])
			if broadcast.Equal(net.IPv4zero) {
				continue
			}
			udpAddr := &net.UDPAddr{IP: broadcast, Port: d.cfg.DiscoveryPort}
			key := udpAddr.String()
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			addrs = append(addrs, udpAddr)
		}
	}
	return addrs
}

func (d *DiscoveryService) writeAnnouncement(conn *net.UDPConn, payload []byte, addrs []*net.UDPAddr) {
	if len(addrs) == 0 {
		addrs = []*net.UDPAddr{{IP: net.IPv4bcast, Port: d.cfg.DiscoveryPort}}
	}
	for _, addr := range addrs {
		if addr == nil {
			continue
		}
		if d.isStopping() {
			return
		}
		if _, err := conn.WriteToUDP(payload, addr); err != nil {
			d.logger.Error("discovery announce to %s: %v", addr, err)
		}
	}
}

func enableBroadcast(conn *net.UDPConn) {
	if conn == nil {
		return
	}
	if raw, err := conn.SyscallConn(); err == nil {
		_ = raw.Control(func(fd uintptr) {
			setBroadcastOption(fd)
		})
	}
}
