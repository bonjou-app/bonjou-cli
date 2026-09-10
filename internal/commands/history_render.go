package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/bonjou-app/bonjou-cli/internal/history"
)

// History rendering builds a fixed-width ASCII table from history.Entry
// records. The table layout is deliberately constrained — the CLI promise
// is that you can copy/paste the output into any terminal-friendly tool
// without ANSI codes or column reflow surprising the reader.

func formatHistoryTable(entries []history.Entry, localUser string) string {
	const (
		colTime      = 19
		colType      = 8
		colPeer      = 18
		colDirection = 10
		colDetails   = 40
		colSize      = 10
	)
	widths := []int{colTime, colType, colPeer, colDirection, colDetails, colSize}
	header := []string{"Time", "Type", "Peer", "Direction", "Details", "Size"}

	var rows [][]string
	for _, entry := range entries {
		peer, direction := describeHistoryDirection(entry, localUser)
		details := describeHistoryDetails(entry)
		size := describeHistorySize(entry)
		rows = append(rows, []string{
			entry.Timestamp.Format("2006-01-02 15:04:05"),
			describeHistoryType(entry),
			peer,
			direction,
			details,
			size,
		})
	}

	topDivider := drawHistoryDivider(widths, '-')
	headerDivider := drawHistoryDivider(widths, '=')
	var buf strings.Builder
	buf.WriteString(topDivider)
	buf.WriteString("\n")
	buf.WriteString(drawHistoryRow(header, widths))
	buf.WriteString("\n")
	buf.WriteString(headerDivider)

	if len(rows) == 0 {
		empty := []string{"-", "-", "-", "-", "History is empty", "-"}
		buf.WriteString("\n")
		buf.WriteString(drawHistoryRow(empty, widths))
		buf.WriteString("\n")
		buf.WriteString(topDivider)
		return buf.String()
	}

	for _, row := range rows {
		buf.WriteString("\n")
		buf.WriteString(drawHistoryRow(row, widths))
		buf.WriteString("\n")
		buf.WriteString(topDivider)
	}
	return buf.String()
}

func drawHistoryDivider(widths []int, fill rune) string {
	var b strings.Builder
	b.WriteString("+")
	for _, width := range widths {
		b.WriteString(strings.Repeat(string(fill), width+2))
		b.WriteString("+")
	}
	return b.String()
}

func drawHistoryRow(cells []string, widths []int) string {
	wrapped := make([][]string, len(cells))
	maxLines := 0
	for idx, cell := range cells {
		width := widths[idx]
		lines := wrapCell(cell, width)
		wrapped[idx] = lines
		if len(lines) > maxLines {
			maxLines = len(lines)
		}
	}
	if maxLines == 0 {
		maxLines = 1
	}
	var b strings.Builder
	for line := 0; line < maxLines; line++ {
		if line > 0 {
			b.WriteString("\n")
		}
		b.WriteString("|")
		for idx, width := range widths {
			text := ""
			if line < len(wrapped[idx]) {
				text = wrapped[idx][line]
			}
			b.WriteString(" ")
			b.WriteString(padCell(text, width))
			b.WriteString(" |")
		}
	}
	return b.String()
}

func describeHistoryType(entry history.Entry) string {
	switch strings.ToLower(entry.Category) {
	case "chat":
		return "Message"
	case "transfer":
		switch strings.ToLower(entry.Kind) {
		case "folder":
			return "Folder"
		case "file":
			return "File"
		default:
			return "Transfer"
		}
	default:
		return titleCaseWord(entry.Category)
	}
}

func describeHistoryDirection(entry history.Entry, localUser string) (string, string) {
	from := strings.TrimSpace(entry.From)
	to := strings.TrimSpace(entry.To)
	local := strings.TrimSpace(localUser)
	switch {
	case local != "" && strings.EqualFold(from, local):
		peer := safePeerLabel(to)
		return peer, "Sent"
	case local != "" && strings.EqualFold(to, local):
		peer := safePeerLabel(from)
		return peer, "Received"
	default:
		peer := safePeerLabel(from)
		if peer == "(unknown)" {
			peer = safePeerLabel(to)
		}
		direction := strings.TrimSpace(from + " → " + to)
		if direction == "→" || direction == "" {
			direction = "-"
		}
		return peer, direction
	}
}

func describeHistoryDetails(entry history.Entry) string {
	switch strings.ToLower(entry.Category) {
	case "chat":
		message := strings.TrimSpace(entry.Message)
		if message == "" {
			message = "(empty message)"
		}
		message = strings.ReplaceAll(message, "\n", " ")
		return fmt.Sprintf("Message %q", message)
	case "transfer":
		label := strings.TrimSpace(entry.Path)
		if label == "" {
			label = "(unknown path)"
		}
		base := filepath.Base(label)
		if base == "." || base == string(os.PathSeparator) {
			base = label
		}
		prefix := "Transfer"
		switch strings.ToLower(entry.Kind) {
		case "file":
			prefix = "File"
		case "folder":
			prefix = "Folder"
		}
		return fmt.Sprintf("%s %s", prefix, base)
	default:
		return strings.TrimSpace(entry.Message)
	}
}

func describeHistorySize(entry history.Entry) string {
	if strings.ToLower(entry.Category) != "transfer" {
		return "-"
	}
	if entry.Size <= 0 {
		return "-"
	}
	return humanBytes(entry.Size)
}

func padCell(value string, width int) string {
	runes := []rune(value)
	if len(runes) >= width {
		return value
	}
	return value + strings.Repeat(" ", width-len(runes))
}

func wrapCell(value string, width int) []string {
	if width <= 0 {
		return []string{""}
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return []string{""}
	}
	runes := []rune(trimmed)
	var lines []string
	for len(runes) > 0 {
		if len(runes) <= width {
			lines = append(lines, strings.TrimSpace(string(runes)))
			break
		}
		split := width
		for i := width; i > 0; i-- {
			r := runes[i-1]
			if unicode.IsSpace(r) {
				split = i
				break
			}
		}
		lines = append(lines, strings.TrimSpace(string(runes[:split])))
		runes = trimLeadingSpaces(runes[split:])
	}
	if len(lines) == 0 {
		lines = append(lines, "")
	}
	return lines
}

func trimLeadingSpaces(runes []rune) []rune {
	idx := 0
	for idx < len(runes) && unicode.IsSpace(runes[idx]) {
		idx++
	}
	return runes[idx:]
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

func titleCaseWord(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "-"
	}
	runes := []rune(strings.ToLower(trimmed))
	runes[0] = unicode.ToUpper(runes[0])
	return string(runes)
}
