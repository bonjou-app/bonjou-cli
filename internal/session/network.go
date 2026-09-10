package session

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bonjou-app/bonjou-cli/internal/config"
	"github.com/bonjou-app/bonjou-cli/internal/events"
)

// LocalIP returns the current LAN IP address advertised by the session.
func (s *Session) LocalIP() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.localIP
}

// RefreshNetworkState updates services when the local interface changes.
func (s *Session) RefreshNetworkState(ip string) bool {
	trimmed := strings.TrimSpace(ip)
	if trimmed == "" {
		return false
	}

	s.mu.Lock()
	if trimmed == s.localIP {
		s.mu.Unlock()
		return false
	}
	s.localIP = trimmed
	s.mu.Unlock()

	if s.Transfer != nil {
		s.Transfer.UpdateLocalEndpoint("", trimmed)
	}
	if s.Discovery != nil {
		s.Discovery.UpdateLocalEndpoint(trimmed, s.Config.ListenPort)
	}
	s.emitStatus(fmt.Sprintf("Network updated: now advertising %s", trimmed))
	return true
}

// StartNetworkWatcher polls for local IP changes and keeps services in sync.
func (s *Session) StartNetworkWatcher(interval time.Duration) func() {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	stop := make(chan struct{})
	var once sync.Once
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		last := s.LocalIP()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				ip, err := config.GetLocalIP()
				if err != nil {
					continue
				}
				ip = strings.TrimSpace(ip)
				if ip == "" {
					continue
				}
				if ip != last {
					if s.RefreshNetworkState(ip) {
						last = ip
					}
				}
			}
		}
	}()
	return func() {
		once.Do(func() {
			close(stop)
		})
	}
}

func (s *Session) emitStatus(message string) {
	if s.Events == nil {
		return
	}
	trimmed := strings.TrimSpace(message)
	if trimmed == "" {
		return
	}
	evt := events.Event{Type: events.Status, Message: trimmed}
	select {
	case s.Events <- evt:
	default:
	}
}
