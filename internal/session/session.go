package session

import (
	"sync"

	"github.com/bonjou-app/bonjou-cli/internal/config"
	"github.com/bonjou-app/bonjou-cli/internal/events"
	"github.com/bonjou-app/bonjou-cli/internal/history"
	"github.com/bonjou-app/bonjou-cli/internal/logger"
	"github.com/bonjou-app/bonjou-cli/internal/network"
	"github.com/bonjou-app/bonjou-cli/internal/queue"
)

// Session wires together Bonjou runtime services.
type Session struct {
	Config    *config.Config
	Logger    *logger.Logger
	History   *history.Manager
	Discovery *network.DiscoveryService
	Transfer  *network.TransferService
	Events    chan events.Event
	Queue     *queue.Manager

	mu      sync.RWMutex
	localIP string
}

func New(cfg *config.Config, log *logger.Logger, hist *history.Manager, disc *network.DiscoveryService, transfer *network.TransferService, events chan events.Event, ip string, queueMgr *queue.Manager) *Session {
	return &Session{
		Config:    cfg,
		Logger:    log,
		History:   hist,
		Discovery: disc,
		Transfer:  transfer,
		Events:    events,
		Queue:     queueMgr,
		localIP:   ip,
	}
}

// Close releases resources associated with the session.
func (s *Session) Close() {
	if s.Transfer != nil {
		s.Transfer.Stop()
	}
	if s.Discovery != nil {
		s.Discovery.Stop()
	}
	if s.Queue != nil {
		_ = s.Queue.Close()
	}
	if s.Logger != nil {
		_ = s.Logger.Close()
	}
}
