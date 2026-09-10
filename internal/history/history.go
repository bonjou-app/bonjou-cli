package history

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bonjou-app/bonjou-cli/internal/config"
)

type Manager struct {
	cfg *config.Config
	mu  sync.Mutex
}

func New(cfg *config.Config) *Manager {
	return &Manager{cfg: cfg}
}

// Entry represents a single history record.
type Entry struct {
	Timestamp time.Time
	Category  string
	Kind      string
	From      string
	To        string
	Message   string
	Path      string
	Size      int64
}

func (m *Manager) chatLogPath() string {
	return filepath.Join(m.cfg.LogDir, "chat.log")
}

func (m *Manager) transferLogPath() string {
	return filepath.Join(m.cfg.LogDir, "transfers.log")
}

func (m *Manager) AppendChat(from, to, message string) error {
	message = strings.ReplaceAll(message, "\r\n", "\n")
	message = strings.ReplaceAll(message, "\r", "\n")
	message = strconv.Quote(message)
	entry := time.Now().Format(time.RFC3339) + " | chat | " + from + " -> " + to + " | " + message + "\n"
	return m.append(m.chatLogPath(), entry)
}

func (m *Manager) AppendTransfer(from, to, path string, size int64, kind string) error {
	entry := time.Now().Format(time.RFC3339) + " | transfer | " + kind + " | " + from + " -> " + to + " | " + path + " | bytes=" + strconv.FormatInt(size, 10) + "\n"
	return m.append(m.transferLogPath(), entry)
}

func (m *Manager) append(path, entry string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
	}()
	_, err = file.WriteString(entry)
	return err
}

func (m *Manager) ReadAll() ([]Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var entries []Entry

	chats, err := readLines(m.chatLogPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, line := range chats {
		entry, err := parseChatEntry(line)
		if err != nil {
			continue
		}
		entries = append(entries, entry)
	}

	transfers, err := readLines(m.transferLogPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, line := range transfers {
		entry, err := parseTransferEntry(line)
		if err != nil {
			continue
		}
		entries = append(entries, entry)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Timestamp.Before(entries[j].Timestamp)
	})

	return entries, nil
}

func (m *Manager) Clear() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	paths := []string{m.chatLogPath(), m.transferLogPath()}
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func readLines(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = file.Close()
	}()
	var lines []string
	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, scanner.Err()
}

func parseChatEntry(line string) (Entry, error) {
	parts := strings.SplitN(line, " | ", 4)
	if len(parts) < 4 {
		return Entry{}, errors.New("invalid chat history entry")
	}
	ts, err := time.Parse(time.RFC3339, parts[0])
	if err != nil {
		return Entry{}, err
	}
	from, to := parseEndpoints(parts[2])
	message := parts[3]
	if unquoted, err := strconv.Unquote(message); err == nil {
		message = unquoted
	}
	return Entry{
		Timestamp: ts,
		Category:  "chat",
		Kind:      "message",
		From:      from,
		To:        to,
		Message:   message,
	}, nil
}

func parseTransferEntry(line string) (Entry, error) {
	parts := strings.SplitN(line, " | ", 6)
	if len(parts) < 6 {
		return Entry{}, errors.New("invalid transfer history entry")
	}
	ts, err := time.Parse(time.RFC3339, parts[0])
	if err != nil {
		return Entry{}, err
	}
	from, to := parseEndpoints(parts[3])
	size, err := strconv.ParseInt(strings.TrimPrefix(parts[5], "bytes="), 10, 64)
	if err != nil {
		size = 0
	}
	return Entry{
		Timestamp: ts,
		Category:  "transfer",
		Kind:      parts[2],
		From:      from,
		To:        to,
		Path:      parts[4],
		Size:      size,
	}, nil
}

func parseEndpoints(segment string) (string, string) {
	parts := strings.SplitN(segment, " -> ", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}
