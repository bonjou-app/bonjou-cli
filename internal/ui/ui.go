package ui

import (
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chzyer/readline"
	runewidth "github.com/mattn/go-runewidth"
	"golang.org/x/term"

	"github.com/bonjou-app/bonjou-cli/internal/commands"
	"github.com/bonjou-app/bonjou-cli/internal/events"
	"github.com/bonjou-app/bonjou-cli/internal/session"
	"github.com/bonjou-app/bonjou-cli/internal/version"
)

const (
	colorReset    = "\033[0m"
	colorPrimary  = "\033[36m"
	colorSuccess  = "\033[32m"
	colorError    = "\033[31m"
	colorMuted    = "\033[90m"
	colorAccent   = "\033[38;2;198;149;255m"
	colorBarEmpty = "\033[38;2;80;80;80m"
	bannerWidth   = 80
	minBarWidth   = 8
	maxBarWidth   = 32
)

var (
	gradientStartRGB = [3]int{161, 130, 253}
	gradientEndRGB   = [3]int{94, 182, 255}
)

var ansiPattern = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// welcomeBannerV2 renders BONJOU in the larry3d figlet font.
// The original welcomeBanner is preserved above for comparison.
var welcomeBannerV2 = []string{
	" ____     _____   __  __   _____  _____   __  __     ",
	"/\\  _`\\  /\\  __`\\/\\ \\/\\ \\ /\\___ \\/\\  __`\\/\\ \\/\\ \\    ",
	"\\ \\ \\L\\ \\\\ \\ \\/\\ \\ \\ `\\\\ \\\\/__/\\ \\ \\ \\/\\ \\ \\ \\ \\ \\   ",
	" \\ \\  _ <'\\ \\ \\ \\ \\ \\ , ` \\  _\\ \\ \\ \\ \\ \\ \\ \\ \\ \\ \\  ",
	"  \\ \\ \\L\\ \\\\ \\ \\_\\ \\ \\ \\`\\ \\/\\ \\_\\ \\ \\ \\_\\ \\ \\ \\_\\ \\ ",
	"   \\ \\____/ \\ \\_____\\ \\_\\ \\_\\ \\____/\\ \\_____\\ \\_____\\",
	"    \\/___/   \\/_____/\\/_/\\/_/\\/___/  \\/_____/\\/_____/",
}

type UI struct {
	session *session.Session
	handler *commands.Handler
	rl      *readline.Instance
	done    chan struct{}

	printMu    sync.Mutex
	progressMu sync.Mutex
	homeDir    string

	progressActive bool
	progressID     string
	progressLine   string
	interactive    bool

	// wizardBuffer captures events that arrive while the wizard owns the
	// alt screen; we replay them on wizard exit so the user does not miss
	// file/message notifications produced during their wizard interaction.
	wizardBufMu sync.Mutex
	wizardBuf   []string
}

const wizardBufCap = 256

func New(session *session.Session, handler *commands.Handler) (*UI, error) {
	interactive := term.IsTerminal(int(os.Stdin.Fd()))
	pasteStdin := newPasteFilterReader(os.Stdin)
	cfg := &readline.Config{
		Prompt:                 colorMuted + "> " + colorReset,
		InterruptPrompt:        colorMuted + "^C" + colorReset + "\n",
		EOFPrompt:              "",
		HistorySearchFold:      true,
		DisableAutoSaveHistory: true,
		UniqueEditLine:         true,
		Stdin:                  pasteStdin,
		Stdout:                 os.Stdout,
		Stderr:                 os.Stderr,
	}
	configureReadline(cfg)
	enableANSI()
	rl, err := readline.NewEx(cfg)
	if err != nil {
		return nil, err
	}
	if interactive {
		_, _ = fmt.Fprint(os.Stdout, enableBracketedPaste)
	} else {
		fmt.Fprintln(os.Stderr, colorMuted+"(Limited terminal detected; line editing shortcuts may be unavailable.)"+colorReset)
	}
	home, _ := os.UserHomeDir()
	u := &UI{
		session:     session,
		handler:     handler,
		rl:          rl,
		done:        make(chan struct{}),
		homeDir:     home,
		interactive: interactive,
	}
	commands.RegisterWizardExitHook(u.flushWizardBuffer)
	return u, nil
}

func configureReadline(cfg *readline.Config) {
	cfg.HistoryLimit = 1024
	cfg.FuncIsTerminal = func() bool {
		return term.IsTerminal(int(os.Stdin.Fd()))
	}
	cfg.FuncGetWidth = func() int {
		width, _, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil || width <= 0 {
			return bannerWidth
		}
		return width
	}
}

// Run starts the interactive Bonjou session.
func (u *UI) Run() {
	defer func() { _ = u.rl.Close() }()
	if u.interactive {
		defer func() { _, _ = fmt.Fprint(os.Stdout, disableBracketedPaste) }()
	}
	u.printWelcome()
	go u.consumeEvents()
	for {
		// Re-enable bracketed paste before each prompt. The wizard (bubbletea)
		// disables it on exit, so without this the next paste after returning
		// from the wizard would split on embedded newlines.
		if u.interactive {
			_, _ = fmt.Fprint(os.Stdout, enableBracketedPaste)
		}
		line, err := u.rl.Readline()
		if err == readline.ErrInterrupt {
			u.writeLine(colorMuted + "^C" + colorReset)
			continue
		}
		if err == io.EOF {
			u.shutdown()
			return
		}
		if err != nil {
			u.writeLine(colorError + "Error reading input: " + err.Error() + colorReset)
			continue
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		_ = u.rl.SaveHistory(line)
		result, err := u.handler.Handle(line)
		if err != nil {
			u.writeResult(colorError + err.Error() + colorReset)
			continue
		}
		if result.Clear {
			u.clearScreen(false)
			continue
		}
		if result.Output != "" {
			u.writeResult(colorSuccess + result.Output + colorReset)
		}
		if result.Quit {
			u.shutdown()
			return
		}
	}
}

func (u *UI) consumeEvents() {
	for {
		select {
		case evt := <-u.session.Events:
			u.renderEvent(evt)
		case <-u.done:
			return
		}
	}
}

func (u *UI) renderEvent(evt events.Event) {
	ts := time.Now().Format("15:04:05")
	switch evt.Type {
	case events.MessageReceived:
		u.writeLine(fmt.Sprintf("%s[%s] Message from %s:%s %s", colorPrimary, ts, safe(evt.From), colorReset, evt.Message))
	case events.MessageSent:
		u.writeLine(fmt.Sprintf("%s[%s] Message sent to %s:%s %s", colorMuted, ts, safe(evt.To), colorReset, evt.Message))
	case events.FileReceived, events.FolderReceived:
		itemKind := "File"
		if evt.Type == events.FolderReceived {
			itemKind = "Folder"
		}
		u.writeLine(fmt.Sprintf("%s[%s] %s received: '%s' from %s -> %s%s", colorPrimary, ts, itemKind, safe(evt.Message), safe(evt.From), evt.Path, colorReset))
	case events.FilePending, events.FolderPending:
		// The prompt specifies: "User1 wants to send a file... " The Message already contains the formatted string.
		u.writeLine(fmt.Sprintf("%s[%s]%s %s", colorAccent, ts, colorReset, evt.Message))
	case events.FileSent, events.FolderSent:
		itemKind := "File"
		if evt.Type == events.FolderSent {
			itemKind = "Folder"
		}
		title := strings.TrimSpace(evt.Title)
		switch title {
		case "File offer sent", "Folder offer sent":
			u.writeLine(fmt.Sprintf("%s[%s] %s offer sent: '%s' to %s (waiting for approval)%s", colorMuted, ts, itemKind, safe(evt.Message), safe(evt.To), colorReset))
		default:
			u.writeLine(fmt.Sprintf("%s[%s] %s upload started: '%s' to %s%s", colorMuted, ts, itemKind, safe(evt.Message), safe(evt.To), colorReset))
		}
	case events.Error:
		msg := safe(evt.Message)
		// Avoid a redundant "ERROR: Delivery failed:" double-label by not
		// prepending "ERROR: " when the message is already a full sentence
		// that starts with a contextual word (capital letter followed by
		// lowercase, not an ALL-CAPS acronym or a bare filename).
		if strings.HasPrefix(msg, "Delivery failed:") ||
			strings.HasPrefix(msg, "Failed to send") ||
			strings.HasPrefix(msg, "File '") ||
			strings.HasPrefix(msg, "Folder '") {
			u.writeLine(fmt.Sprintf("%s[%s] ✗ %s%s", colorError, ts, msg, colorReset))
		} else {
			u.writeLine(fmt.Sprintf("%s[%s] ERROR: %s%s", colorError, ts, msg, colorReset))
		}
	case events.Status:
		if strings.EqualFold(strings.TrimSpace(evt.Title), "Delivery confirmed") || strings.HasPrefix(strings.TrimSpace(evt.Message), "Delivered:") {
			u.writeLine(fmt.Sprintf("%s[%s] %s%s", colorSuccess, ts, safe(evt.Message), colorReset))
			return
		}
		u.writeLine(fmt.Sprintf("%s[%s] %s%s", colorMuted, ts, safe(evt.Message), colorReset))
	case events.Progress:
		u.renderProgress(evt)
	}
}

func (u *UI) printWelcome() {
	sep := "◆─────────────────────────◆─────────────────────────◆"
	u.writeLine(colorAccent + displayCenter(sep, bannerWidth) + colorReset)
	for _, line := range welcomeBannerV2 {
		u.writeLine(gradientLine(centerLine(line, bannerWidth)))
	}
	u.writeLine(colorAccent + displayCenter(sep, bannerWidth) + colorReset)
	tagline := "Terminal LAN chat & transfers  |  encrypted  |  fast"
	u.writeLine(colorMuted + centerLine(tagline, bannerWidth) + colorReset)
	u.writeLine("")
	u.writeLine(fmt.Sprintf("%s🌐 Welcome to Bonjou v%s%s", colorPrimary, version.Version, colorReset))
	u.writeLine(fmt.Sprintf("%s👤 User:%s %s | IP: %s", colorMuted, colorReset, u.session.Config.Username, u.session.LocalIP()))
	u.writeLine(fmt.Sprintf("%s📡 LAN:%s Connected", colorMuted, colorReset))
	u.writeLine("Type @help for commands.")
}

func (u *UI) clearScreen(printWelcome bool) {
	if u.rl == nil {
		fmt.Print("\033[2J\033[H")
	} else {
		u.printMu.Lock()
		_, _ = fmt.Fprint(u.rl.Stdout(), "\033[2J\033[H")
		u.printMu.Unlock()
		u.rl.Refresh()
	}
	u.progressMu.Lock()
	u.progressActive = false
	u.progressLine = ""
	u.progressID = ""
	u.progressMu.Unlock()
	if printWelcome {
		u.printWelcome()
	}
}

func (u *UI) renderProgress(evt events.Event) {
	ps := evt.Progress
	if ps == nil || ps.Total <= 0 {
		return
	}
	if strings.EqualFold(ps.Direction, "receive") {
		return
	}
	percent := float64(ps.Current) / float64(ps.Total) * 100
	if percent < 0 {
		percent = 0
	}
	if percent > 100 {
		percent = 100
	}
	now := time.Now()
	line := u.formatProgressLine(ps, percent, ps.Done, now)
	if ps.Done {
		u.finishProgressLine(ps.ID, line)
		return
	}
	u.updateProgressLine(ps.ID, line)
}

func (u *UI) shutdown() {
	select {
	case <-u.done:
	default:
		close(u.done)
	}
	u.writeLine(colorMuted + "Ending Bonjou session. Goodbye!" + colorReset)
}

// writeResult prints one command's output and then a blank line, so that
// running two commands in a row does not produce one undifferentiated
// block of text above the next prompt.
//
// Trailing newlines are trimmed first: a result that already ends in one
// would otherwise leave a double gap, while a result with deliberate
// internal blank lines keeps them.
func (u *UI) writeResult(text string) {
	u.writeLine(strings.TrimRight(text, "\n"))
	u.writeLine("")
}

func (u *UI) writeLine(line string) {
	// While the wizard owns the alt screen, any write to the terminal
	// would corrupt its rendering and leave artifacts on the main screen
	// once the alt screen exits. Buffer the event instead — the on-exit
	// hook (registered in New) flushes it back to the main scrollback so
	// the user sees notifications produced during their wizard session.
	//
	// The buffer is the record; WizardNotify is the peek. Without the
	// second call an incoming message stayed invisible until the wizard
	// closed, so somebody filling in a form had no idea they had been
	// spoken to. The alt screen is discarded on exit, which is why the
	// line is still buffered for the scrollback rather than only shown.
	if commands.WizardRenderActive() {
		u.bufferWizardLine(line)
		commands.WizardNotify(line)
		return
	}
	u.emitLine(line)
}

func (u *UI) emitLine(line string) {
	u.progressMu.Lock()
	active := u.progressActive
	progressLine := u.progressLine
	u.progressMu.Unlock()

	if u.rl == nil {
		fmt.Printf("\r\033[K%s\n", line)
		if active {
			fmt.Printf("%s", progressLine)
		}
		return
	}

	u.printMu.Lock()
	stderr := u.rl.Stderr()
	_, _ = fmt.Fprint(stderr, "\r\033[K")
	_, _ = fmt.Fprintf(stderr, "%s\n", line)
	if active {
		_, _ = fmt.Fprintf(stderr, "%s", progressLine)
	}
	u.printMu.Unlock()
}

func (u *UI) bufferWizardLine(line string) {
	u.wizardBufMu.Lock()
	defer u.wizardBufMu.Unlock()
	// Drop the oldest entries past the cap so unbounded wizard sessions
	// (e.g. a user who walks away with the wizard open) can't grow memory
	// without limit. The newest events are kept because they're usually
	// the ones the user wants to react to.
	if len(u.wizardBuf) >= wizardBufCap {
		u.wizardBuf = u.wizardBuf[len(u.wizardBuf)-wizardBufCap+1:]
	}
	u.wizardBuf = append(u.wizardBuf, line)
}

func (u *UI) flushWizardBuffer() {
	u.wizardBufMu.Lock()
	pending := u.wizardBuf
	u.wizardBuf = nil
	u.wizardBufMu.Unlock()
	if len(pending) == 0 {
		return
	}
	u.emitLine(colorMuted + fmt.Sprintf("— Replaying %d event(s) buffered during wizard —", len(pending)) + colorReset)
	for _, line := range pending {
		u.emitLine(line)
	}
}

func (u *UI) updateProgressLine(id, line string) {
	u.progressMu.Lock()
	u.progressActive = true
	u.progressID = id
	u.progressLine = line
	u.progressMu.Unlock()

	// A progress tick is a carriage return and an erase, straight to the
	// terminal, several times a second. Sent while the wizard owns the alt
	// screen it lands in the middle of bubbletea's canvas and shreds it.
	// The state above is still updated, so the bar resumes correctly once
	// the wizard closes; only the drawing is withheld. Ticks are dropped
	// rather than buffered because replaying a thousand of them on exit
	// would bury the events worth reading.
	if commands.WizardRenderActive() {
		return
	}

	if u.rl == nil {
		fmt.Printf("\r\033[J%s", line)
		return
	}

	u.printMu.Lock()
	_, _ = fmt.Fprintf(u.rl.Stderr(), "\r\033[J%s", line)
	u.printMu.Unlock()
}

func (u *UI) finishProgressLine(id, line string) {
	var resume bool
	var resumeID string
	var resumeLine string

	u.progressMu.Lock()
	if u.progressActive && u.progressID != id {
		resume = true
		resumeID = u.progressID
		resumeLine = u.progressLine
	}
	if u.progressID == id {
		u.progressActive = false
		u.progressLine = ""
		u.progressID = ""
	}
	u.progressMu.Unlock()

	// The completed line is the one worth keeping, unlike the ticks that
	// led to it, so it goes through writeLine and gets both the wizard's
	// notice block and the buffer that replays into the scrollback.
	if commands.WizardRenderActive() {
		u.writeLine(line)
		return
	}

	if u.rl == nil {
		fmt.Printf("\r\033[J%s\n", line)
	} else {
		u.printMu.Lock()
		_, _ = fmt.Fprintf(u.rl.Stderr(), "\r\033[J%s\n", line)
		u.printMu.Unlock()
	}

	if resume {
		u.updateProgressLine(resumeID, resumeLine)
	}
}

func safe(in string) string {
	if strings.TrimSpace(in) == "" {
		return "(unknown)"
	}
	return in
}

func centerLine(line string, width int) string {
	trimmed := strings.TrimRight(line, "\n")
	if len(trimmed) >= width {
		return trimmed
	}
	pad := (width - len(trimmed)) / 2
	if pad < 0 {
		pad = 0
	}
	return strings.Repeat(" ", pad) + trimmed
}

// displayCenter centers s within width using terminal display-cell width
// (via runewidth) rather than byte length, so multi-byte Unicode characters
// (e.g. box-drawing glyphs) are aligned correctly.
func displayCenter(s string, width int) string {
	sw := runewidth.StringWidth(s)
	if sw >= width {
		return s
	}
	pad := (width - sw) / 2
	return strings.Repeat(" ", pad) + s
}

func (u *UI) formatProgressLine(ps *events.ProgressState, percent float64, done bool, now time.Time) string {
	width := u.terminalWidth()
	barWidth := determineBarWidth(width)
	labelLimit := labelCharacterLimit(width, barWidth)
	peerLimit := labelLimit / 2
	if peerLimit < 8 {
		peerLimit = 8
	}
	target := u.progressTarget(ps, labelLimit)
	peer := progressPeerLabel(ps.Direction, ps.Peer, peerLimit)
	maxWidth := width - 2
	if maxWidth < 20 {
		maxWidth = width
	}
	if done {
		elapsedLabel := "--:--"
		if !ps.StartedAt.IsZero() {
			elapsedLabel = formatDuration(now.Sub(ps.StartedAt))
		}
		metrics := fmt.Sprintf("%s100%%%s • %sTime%s %s",
			colorPrimary, colorReset,
			colorMuted, colorReset,
			elapsedLabel,
		)
		summary := fmt.Sprintf("%s⇡%s %s %s", colorPrimary, colorReset, progressCompletedVerb(ps.Direction), target)
		if peer != "" {
			summary += " " + peer
		}
		if ps.Total > 0 {
			summary += fmt.Sprintf(" • %s", humanBytes(ps.Total))
		}
		return composeProgressLine(summary, metrics, 100, barWidth, maxWidth)
	}
	eta := formatETA(ps.StartedAt, now, percent)
	metrics := fmt.Sprintf("%s%5.1f%%%s • %sETA%s %s",
		colorPrimary, percent, colorReset,
		colorMuted, colorReset,
		eta,
	)
	summary := fmt.Sprintf("%s⇢%s %s %s", colorAccent, colorReset, progressActiveVerb(ps.Direction), target)
	if peer != "" {
		summary += " " + peer
	}
	return composeProgressLine(summary, metrics, percent, barWidth, maxWidth)
}

func (u *UI) progressTarget(ps *events.ProgressState, limit int) string {
	var label string
	switch strings.ToLower(ps.Kind) {
	case "file":
		label = "File"
	case "folder":
		label = "Folder"
	case "message":
		label = "Message"
	default:
		label = "Transfer"
	}
	glyph := progressKindGlyph(ps.Kind, ps.Direction)
	colored := colorPrimary + label + colorReset
	if glyph == "" {
		return colored
	}
	return glyph + " " + colored
}

func progressKindGlyph(kind, direction string) string {
	switch strings.ToLower(kind) {
	case "folder":
		if strings.EqualFold(direction, "receive") {
			return "🗃️"
		}
		return "🗂️"
	case "file":
		if strings.EqualFold(direction, "receive") {
			return "📥"
		}
		return "📤"
	case "message":
		return "💬"
	default:
		if strings.EqualFold(direction, "receive") {
			return "⬇"
		}
		return "⬆"
	}
}

func progressPeerLabel(direction, peer string, limit int) string {
	if strings.TrimSpace(peer) == "" {
		return ""
	}
	arrow := "→"
	if strings.EqualFold(direction, "receive") {
		arrow = "←"
	}
	name := truncateMiddle(strings.TrimSpace(peer), limit)
	return fmt.Sprintf("%s%s%s %s%s%s", colorMuted, arrow, colorReset, colorPrimary, name, colorReset)
}

func progressActiveVerb(direction string) string {
	if strings.EqualFold(direction, "receive") {
		return "Receiving"
	}
	return "Sending"
}

func progressCompletedVerb(direction string) string {
	if strings.EqualFold(direction, "receive") {
		return "Received"
	}
	return "Uploaded"
}

func buildGradientBar(percent float64, width int) string {
	if width <= 0 {
		width = 16
	}
	filled := int(math.Round(percent / 100 * float64(width)))
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	var sb strings.Builder
	sb.WriteString(colorMuted + "▏" + colorReset)
	for i := 0; i < width; i++ {
		if i < filled {
			ratio := 0.0
			if width > 1 {
				ratio = float64(i) / float64(width-1)
			}
			sb.WriteString(gradientColor(ratio))
			sb.WriteRune('█')
		} else {
			sb.WriteString(colorBarEmpty)
			sb.WriteRune('░')
		}
	}
	sb.WriteString(colorReset)
	sb.WriteString(colorMuted + "▕" + colorReset)
	return sb.String()
}

func gradientColor(ratio float64) string {
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	r := int(float64(gradientStartRGB[0]) + ratio*float64(gradientEndRGB[0]-gradientStartRGB[0]))
	g := int(float64(gradientStartRGB[1]) + ratio*float64(gradientEndRGB[1]-gradientStartRGB[1]))
	b := int(float64(gradientStartRGB[2]) + ratio*float64(gradientEndRGB[2]-gradientStartRGB[2]))
	return fmt.Sprintf("\033[38;2;%d;%d;%dm", r, g, b)
}

// gradientLine applies a left-to-right purple→blue gradient to every
// character in s using the same palette as the progress bar.
func gradientLine(s string) string {
	runes := []rune(s)
	n := len(runes)
	if n == 0 {
		return s
	}
	var sb strings.Builder
	for i, r := range runes {
		ratio := 0.0
		if n > 1 {
			ratio = float64(i) / float64(n-1)
		}
		sb.WriteString(gradientColor(ratio))
		sb.WriteRune(r)
	}
	sb.WriteString(colorReset)
	return sb.String()
}

func humanBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	value := float64(n)
	idx := 0
	for value >= 1024 && idx < len(units)-1 {
		value /= 1024
		idx++
	}
	if value >= 10 || idx == 0 {
		return fmt.Sprintf("%.0f %s", value, units[idx])
	}
	return fmt.Sprintf("%.1f %s", value, units[idx])
}

func (u *UI) terminalWidth() int {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err == nil && width > 0 {
		return width
	}
	return bannerWidth
}

func truncateMiddle(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	head := limit / 2
	tail := limit - head - 1
	if tail < 0 {
		tail = 0
	}
	return string(runes[:head]) + "…" + string(runes[len(runes)-tail:])
}

func formatETA(start time.Time, now time.Time, percent float64) string {
	progress := percent / 100
	if progress <= 0 || start.IsZero() {
		return "--:--"
	}
	elapsed := now.Sub(start)
	if elapsed <= 0 {
		return "--:--"
	}
	remaining := time.Duration(float64(elapsed) * (1 - progress) / progress)
	if remaining < 0 {
		remaining = 0
	}
	return formatDuration(remaining)
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d >= 24*time.Hour {
		days := int(d / (24 * time.Hour))
		return fmt.Sprintf("%dd", days)
	}
	hours := int(d / time.Hour)
	minutes := int(d/time.Minute) % 60
	seconds := int(d/time.Second) % 60
	if hours > 0 {
		return fmt.Sprintf("%d:%02d:%02d", hours, minutes, seconds)
	}
	return fmt.Sprintf("%02d:%02d", minutes, seconds)
}

func determineBarWidth(width int) int {
	if width <= 0 {
		width = bannerWidth
	}
	bar := width / 4
	if bar < minBarWidth {
		bar = minBarWidth
	}
	if bar > maxBarWidth {
		bar = maxBarWidth
	}
	return bar
}

func labelCharacterLimit(width, barWidth int) int {
	if width <= 0 {
		width = bannerWidth
	}
	if barWidth < minBarWidth {
		barWidth = minBarWidth
	}
	available := width - barWidth - 28
	if available < 8 {
		available = 8
	}
	maxLabel := width - 10
	if maxLabel < 8 {
		maxLabel = 8
	}
	if available > maxLabel {
		available = maxLabel
	}
	return available
}

func composeProgressLine(summary, metrics string, percent float64, barWidth, maxWidth int) string {
	if maxWidth <= 0 {
		maxWidth = bannerWidth
	}
	if barWidth < minBarWidth {
		barWidth = minBarWidth
	}
	if barWidth > maxBarWidth {
		barWidth = maxBarWidth
	}
	const separator = "  "
	currentBarWidth := barWidth
	var line string
	for {
		bar := buildGradientBar(percent, currentBarWidth)
		content := summary + separator + bar + separator + metrics
		line = "\r" + content
		if visibleWidth(line) <= maxWidth {
			break
		}
		if currentBarWidth > minBarWidth {
			currentBarWidth--
			continue
		}
		compacted := compactMetrics(metrics)
		if compacted != metrics {
			metrics = compacted
			continue
		}
		otherWidth := visibleWidth(separator + bar + separator + metrics)
		remaining := maxWidth - otherWidth
		if remaining < 0 {
			remaining = 0
		}
		summary = truncateWithANSI(summary, remaining)
		content = summary + separator + bar + separator + metrics
		if visibleWidth("\r"+content) <= maxWidth {
			line = "\r" + content
			break
		}
		content = truncateWithANSI(content, maxWidth)
		line = "\r" + content
		break
	}
	return line
}

func compactMetrics(metrics string) string {
	idx := strings.Index(metrics, "•")
	if idx == -1 {
		return metrics
	}
	trimmed := metrics[:idx]
	trimmed = strings.TrimRight(trimmed, " ")
	if trimmed == "" {
		return metrics
	}
	if strings.Contains(trimmed, "\033[") && !strings.HasSuffix(trimmed, colorReset) {
		trimmed += colorReset
	}
	return trimmed
}

func visibleWidth(s string) int {
	if s == "" {
		return 0
	}
	clean := stripANSI(s)
	clean = strings.ReplaceAll(clean, "\r", "")
	clean = strings.ReplaceAll(clean, "\n", "")
	return runewidth.StringWidth(clean)
}

func stripANSI(s string) string {
	if s == "" {
		return ""
	}
	return ansiPattern.ReplaceAllString(s, "")
}

func truncateWithANSI(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	width := 0
	var b strings.Builder
	hasANSI := false
	for i := 0; i < len(s); {
		if s[i] == '\033' && i+1 < len(s) {
			end := i + 1
			for end < len(s) {
				ch := s[end]
				if ch >= '@' && ch <= '~' {
					end++
					break
				}
				end++
			}
			if end > len(s) {
				end = len(s)
			}
			b.WriteString(s[i:end])
			i = end
			hasANSI = true
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			if width+1 > limit {
				break
			}
			width++
			b.WriteByte(s[i])
			i++
			continue
		}
		if r == '\r' || r == '\n' {
			b.WriteRune(r)
			i += size
			continue
		}
		rw := runewidth.RuneWidth(r)
		if rw == 0 {
			rw = 1
		}
		if width+rw > limit {
			break
		}
		width += rw
		b.WriteRune(r)
		i += size
	}
	result := b.String()
	if hasANSI && !strings.HasSuffix(result, colorReset) {
		result += colorReset
	}
	return result
}
