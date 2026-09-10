// Package commands implements the @-prefixed command dispatcher used by
// the Bonjou TUI. The top-level Handler.Handle method parses a raw input
// line and dispatches to the appropriate cmd* method.
//
// The actual command implementations are split across focused files:
//
//   - transfer_cmds.go   — @send / @file / @folder / @multi / @broadcast
//   - wizard.go          — interactive @wizard and helpers
//   - queue_commands.go  — @queue / @approve / @reject / @view
//   - known_peers_cmds.go — @fingerprint / @trust / @forget / @known
//   - setpath.go         — @setpath with system-path guarding
//   - peer.go            — peer resolution, label, username utilities
//   - parse.go           — input sanitisation and arg splitting
//   - help.go            — @help text
//   - history_render.go  — @history table renderer
package commands

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bonjou-app/bonjou-cli/internal/session"
)

var ErrUnknownCommand = errors.New("unknown command")

// Result carries command execution outcome back to the UI.
type Result struct {
	Output string
	Clear  bool
	Quit   bool
}

type Handler struct {
	session *session.Session
}

func New(session *session.Session) *Handler {
	return &Handler{session: session}
}

// asOutput reports a condition to the user through the normal output
// stream rather than as a command failure.
//
// The console renders a returned error in its error style; these are
// situations the user can correct, such as a bad queue id or a path that
// does not exist, and they read better as ordinary output carrying the
// usage hint. The nil error is the point, not an oversight.
func asOutput(message string) (Result, error) {
	return Result{Output: message}, nil
}

// Handle parses command input and executes the matching action.
func (h *Handler) Handle(input string) (Result, error) {
	trimmed := sanitizeCommandInput(input)
	if trimmed == "" {
		return Result{}, nil
	}
	if !strings.HasPrefix(trimmed, "@") {
		return Result{Output: "Commands must start with @. Type @help for options."}, nil
	}
	parts := strings.Fields(trimmed)
	cmd := strings.TrimPrefix(parts[0], "@")
	args := strings.TrimSpace(strings.TrimPrefix(trimmed, parts[0]))

	switch cmd {
	case "help":
		return Result{Output: helpText()}, nil
	case "whoami":
		return h.cmdWhoAmI()
	case "users":
		return h.cmdUsers()
	case "send":
		return h.cmdSend(parts, args)
	case "file":
		return h.cmdFile(parts, args)
	case "folder":
		return h.cmdFolder(parts, args)
	case "multi":
		return h.cmdMulti(parts, args)
	case "broadcast":
		return h.cmdBroadcast(args)
	case "wizard":
		return h.cmdWizard()
	case "history":
		return h.cmdHistory()
	case "setpath":
		return h.cmdSetPath(args)
	case "setname":
		return h.cmdSetName(args)
	case "status":
		return h.cmdStatus()
	case "clear":
		return h.cmdClear(args)
	case "exit", "quit":
		return h.cmdExit()
	case "exit!", "quit!":
		return Result{Quit: true}, nil
	case "queue":
		return h.cmdQueue()
	case "approve":
		return h.cmdApprove(args)
	case "reject":
		return h.cmdReject(args)
	case "approveAll":
		return h.cmdApproveAll()
	case "rejectAll":
		return h.cmdRejectAll()
	case "view":
		return h.cmdView(args)
	case "fingerprint":
		return h.cmdFingerprint(args)
	case "trust":
		return h.cmdTrust(args)
	case "forget":
		return h.cmdForget(args)
	case "known":
		return h.cmdKnown()
	default:
		return Result{}, ErrUnknownCommand
	}
}

func (h *Handler) cmdWhoAmI() (Result, error) {
	cfg := h.session.Config
	msg := fmt.Sprintf("Username: %s\nIP: %s\nListen port: %d", cfg.Username, h.session.LocalIP(), cfg.ListenPort)
	return Result{Output: msg}, nil
}

func (h *Handler) cmdUsers() (Result, error) {
	peers := h.session.Discovery.ListPeers()
	if h.session.Discovery != nil {
		go h.session.Discovery.ForceAnnounce()
	}
	if len(peers) == 0 {
		return Result{Output: "No active users discovered."}, nil
	}
	var lines []string
	for _, peer := range peers {
		seen := seenLabel(peer.LastSeen)
		lines = append(lines, fmt.Sprintf("%s (%s) • %s", safePeerLabel(peer.Username), peer.IP, seen))
	}
	return Result{Output: strings.Join(lines, "\n")}, nil
}

func (h *Handler) cmdHistory() (Result, error) {
	entries, err := h.session.History.ReadAll()
	if err != nil {
		return Result{}, err
	}
	if len(entries) == 0 {
		return Result{Output: "History is empty."}, nil
	}
	render := formatHistoryTable(entries, h.session.Config.Username)
	return Result{Output: render}, nil
}

func (h *Handler) cmdSetName(arg string) (Result, error) {
	name := strings.TrimSpace(arg)
	if name == "" {
		return Result{Output: "Usage: @setname <username>"}, nil
	}
	if strings.ContainsAny(name, "\n\r") {
		return Result{Output: "Username cannot contain newlines."}, nil
	}
	sanitised, changed := sanitiseUsername(name)
	if sanitised == "" {
		return Result{Output: "Username cannot be blank."}, nil
	}
	if len(sanitised) > 64 {
		return Result{Output: "Username must be 64 characters or fewer."}, nil
	}
	cfg := h.session.Config
	if cfg == nil {
		return Result{}, errors.New("configuration not loaded")
	}
	if cfg.Username == sanitised {
		return Result{Output: fmt.Sprintf("Username already set to %s", sanitised)}, nil
	}
	resolvedName := sanitised
	collisionResolved := false
	if h.session.Discovery != nil {
		peers := h.session.Discovery.ListPeers()
		resolvedName, collisionResolved = resolveUniqueUsername(sanitised, h.session.LocalIP(), peers)
	}
	old := cfg.Username
	cfg.Username = resolvedName
	if err := cfg.Save(); err != nil {
		cfg.Username = old
		return Result{}, err
	}
	if h.session.Transfer != nil {
		h.session.Transfer.UpdateLocalUser(resolvedName)
	}
	if h.session.Discovery != nil {
		h.session.Discovery.UpdateLocalUser(resolvedName)
		h.session.Discovery.ForceAnnounce()
	}
	msg := fmt.Sprintf("Username updated to %s", resolvedName)
	if changed {
		msg += " (spaces converted to '-')"
	}
	if collisionResolved {
		msg += fmt.Sprintf(" (requested name %s was in use)", sanitised)
	}
	return Result{Output: msg}, nil
}

func (h *Handler) cmdStatus() (Result, error) {
	cfg := h.session.Config
	peers := h.session.Discovery.ListPeers()
	fileCount := 0
	folderCount := 0
	if h.session.Queue != nil {
		fileCount = len(h.session.Queue.ListFiles())
		folderCount = len(h.session.Queue.ListFolders())
	}
	lines := []string{
		fmt.Sprintf("Username: %s", cfg.Username),
		fmt.Sprintf("Local IP: %s", h.session.LocalIP()),
		fmt.Sprintf("Listen port: %d", cfg.ListenPort),
		fmt.Sprintf("Discovery port: %d", cfg.DiscoveryPort),
		fmt.Sprintf("Discovered peers: %d", len(peers)),
		fmt.Sprintf("Pending approvals: %d (%d files, %d folders)", fileCount+folderCount, fileCount, folderCount),
		fmt.Sprintf("Receive path: %s", cfg.SaveDir),
	}
	return Result{Output: strings.Join(lines, "\n")}, nil
}

func (h *Handler) cmdClear(arg string) (Result, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return Result{Clear: true}, nil
	}
	if strings.EqualFold(arg, "history") {
		if err := h.session.History.Clear(); err != nil {
			return Result{}, err
		}
		return Result{Output: "History cleared."}, nil
	}
	return Result{Output: "Usage: @clear [history]"}, nil
}

func (h *Handler) cmdExit() (Result, error) {
	if h == nil || h.session == nil || h.session.Queue == nil {
		return Result{Quit: true}, nil
	}

	fileCount := len(h.session.Queue.ListFiles())
	folderCount := len(h.session.Queue.ListFolders())
	total := fileCount + folderCount
	if total == 0 {
		return Result{Quit: true}, nil
	}

	return Result{
		Output: fmt.Sprintf(
			"There are %d pending approvals (%d files, %d folders). Review them with @queue, or force quit with @exit!.",
			total,
			fileCount,
			folderCount,
		),
	}, nil
}
