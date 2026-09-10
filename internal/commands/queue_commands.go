package commands

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bonjou-app/bonjou-cli/internal/format"
	"github.com/bonjou-app/bonjou-cli/internal/queue"
)

type pendingKind string

const (
	pendingKindFile   pendingKind = "file"
	pendingKindFolder pendingKind = "folder"
)

type pendingSummary struct {
	ID        int
	Kind      pendingKind
	Name      string
	Sender    string
	SenderIP  string
	Size      int64
	Timestamp time.Time
}

func (h *Handler) cmdQueue() (Result, error) {
	items := h.pendingSummaries()
	if len(items) == 0 {
		return Result{Output: "Approval queue is empty."}, nil
	}

	var sb strings.Builder
	sb.WriteString("\033[36mPending Approvals:\033[0m\n")
	for _, item := range items {
		kindLabel := "File"
		nameLabel := item.Name
		if item.Kind == pendingKindFolder {
			kindLabel = "Folder"
			nameLabel += "/"
		}
		sender := item.Sender
		if strings.TrimSpace(item.SenderIP) != "" {
			sender = fmt.Sprintf("%s (%s)", item.Sender, item.SenderIP)
		}
		fmt.Fprintf(&sb, "  [%d] %s • %s • from %s • %s • queued %s\n",
			item.ID,
			kindLabel,
			nameLabel,
			sender,
			formatSize(item.Size),
			queuedLabel(item.Timestamp),
		)
	}
	sb.WriteString("\nUse @view <ID>, @approve <ID>, @reject <ID>, @approveAll, or @rejectAll")
	return Result{Output: sb.String()}, nil
}

func (h *Handler) cmdApprove(args string) (Result, error) {
	queueID, err := parseSingleQueueID(args, "Usage: @approve <Queue_ID>")
	if err != nil {
		return asOutput(err.Error())
	}

	kind, approvedPath, approvedName, actionErr := h.approvePendingByID(queueID)
	if actionErr != nil {
		return Result{Output: fmt.Sprintf("Error: %v", actionErr)}, nil
	}

	switch kind {
	case pendingKindFile:
		return Result{Output: fmt.Sprintf("Approved file '%s'. Download requested to %s", approvedName, approvedPath)}, nil
	case pendingKindFolder:
		return Result{Output: fmt.Sprintf("Approved folder '%s'. Download requested to %s", approvedName, approvedPath)}, nil
	default:
		return Result{Output: "Error: unknown queue item type"}, nil
	}
}

func (h *Handler) cmdReject(args string) (Result, error) {
	queueID, err := parseSingleQueueID(args, "Usage: @reject <Queue_ID>")
	if err != nil {
		return asOutput(err.Error())
	}

	kind, rejectedName, actionErr := h.rejectPendingByID(queueID)
	if actionErr != nil {
		return Result{Output: fmt.Sprintf("Error: %v", actionErr)}, nil
	}

	switch kind {
	case pendingKindFile:
		return Result{Output: fmt.Sprintf("Rejected file '%s'.", rejectedName)}, nil
	case pendingKindFolder:
		return Result{Output: fmt.Sprintf("Rejected folder '%s'.", rejectedName)}, nil
	default:
		return Result{Output: "Error: unknown queue item type"}, nil
	}
}

func (h *Handler) cmdApproveAll() (Result, error) {
	items := h.pendingSummaries()
	if len(items) == 0 {
		return Result{Output: "Approval queue is empty."}, nil
	}

	count := 0
	var failures []string
	for _, item := range items {
		kind, _, name, err := h.approvePendingByID(item.ID)
		if err != nil {
			failures = append(failures, fmt.Sprintf("[%d] %s: %v", item.ID, item.Name, err))
			continue
		}
		count++
		_ = kind
		_ = name
	}

	if len(failures) > 0 {
		return Result{Output: fmt.Sprintf("Approved %d queued items. Failed %d:\n%s", count, len(failures), strings.Join(failures, "\n"))}, nil
	}
	return Result{Output: fmt.Sprintf("Approved %d queued items.", count)}, nil
}

func (h *Handler) cmdRejectAll() (Result, error) {
	items := h.pendingSummaries()
	if len(items) == 0 {
		return Result{Output: "Approval queue is empty."}, nil
	}

	count := 0
	var failures []string
	for _, item := range items {
		kind, _, err := h.rejectPendingByID(item.ID)
		if err != nil {
			failures = append(failures, fmt.Sprintf("[%d] %s: %v", item.ID, item.Name, err))
			continue
		}
		count++
		_ = kind
	}

	if len(failures) > 0 {
		return Result{Output: fmt.Sprintf("Rejected %d queued items. Failed %d:\n%s", count, len(failures), strings.Join(failures, "\n"))}, nil
	}
	return Result{Output: fmt.Sprintf("Rejected %d queued items.", count)}, nil
}

func (h *Handler) cmdView(args string) (Result, error) {
	indices, err := parseCommaIndices(args)
	if err != nil || len(indices) == 0 {
		return asOutput("Usage: @view <Queue_ID>")
	}
	if len(indices) != 1 {
		return Result{Output: "Metadata-first approvals do not support nested view indices. Use @view <Queue_ID>."}, nil
	}

	if file, err := h.session.Queue.GetFile(indices[0]); err == nil {
		sender := file.Sender
		if strings.TrimSpace(file.SenderIP) != "" {
			sender = fmt.Sprintf("%s (%s)", file.Sender, file.SenderIP)
		}
		destPath := queue.UniquePath(filepath.Join(h.session.Config.ReceivedFilesDir, file.Name))
		lines := []string{
			fmt.Sprintf("Pending file [%d]", file.ID),
			fmt.Sprintf("Name: %s", file.Name),
			fmt.Sprintf("From: %s", sender),
			fmt.Sprintf("Size: %s", formatSize(file.Size)),
			fmt.Sprintf("Queued: %s", queuedLabel(file.Timestamp)),
			fmt.Sprintf("Destination: %s", destPath),
		}
		if preview := strings.TrimSpace(file.Preview); preview != "" {
			lines = append(lines, fmt.Sprintf("Preview: %s", preview))
		}
		lines = append(lines, fmt.Sprintf("Next: @approve %d or @reject %d", file.ID, file.ID))
		lines = append(lines, "Status: not downloaded yet")
		return Result{Output: strings.Join(lines, "\n")}, nil
	}

	queueID := indices[0]
	f, err := h.session.Queue.GetFolder(queueID)
	if err != nil {
		return Result{Output: fmt.Sprintf("Error: %v", err)}, nil
	}

	destPath := queue.UniquePath(filepath.Join(h.session.Config.ReceivedFoldersDir, f.Name))
	var sb strings.Builder
	fmt.Fprintf(&sb, "\033[36mPending Folder [%d] '%s/':\033[0m\n", queueID, f.Name)
	fmt.Fprintf(&sb, "From: %s", f.Sender)
	if strings.TrimSpace(f.SenderIP) != "" {
		fmt.Fprintf(&sb, " (%s)", f.SenderIP)
	}
	fmt.Fprintf(&sb, "\nSize: %s\n", formatSize(f.Size))
	fmt.Fprintf(&sb, "Queued: %s\n", queuedLabel(f.Timestamp))
	fmt.Fprintf(&sb, "Destination: %s\n", destPath)
	fmt.Fprintf(&sb, "Next: @approve %d or @reject %d\n", queueID, queueID)
	sb.WriteString("Status: not downloaded yet")

	preview := strings.TrimSpace(f.Preview)
	if preview != "" {
		sb.WriteString("\n\nManifest:\n")
		for _, line := range strings.Split(preview, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			sb.WriteString("  ")
			sb.WriteString(line)
			sb.WriteString("\n")
		}
		return Result{Output: strings.TrimRight(sb.String(), "\n")}, nil
	}

	sb.WriteString("\n\n  (no manifest available)")
	return Result{Output: sb.String()}, nil
}

func parseCommaIndices(args string) ([]int, error) {
	parts := strings.Split(args, ",")
	var indices []int
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		val, err := strconv.Atoi(trimmed)
		if err != nil {
			return nil, err
		}
		indices = append(indices, val)
	}
	return indices, nil
}

func parseSingleQueueID(args, usage string) (int, error) {
	indices, err := parseCommaIndices(args)
	if err != nil || len(indices) == 0 {
		return 0, errors.New(usage)
	}
	if len(indices) != 1 {
		return 0, errors.New("use exactly one queue ID")
	}
	return indices[0], nil
}

func (h *Handler) approvePendingByID(queueID int) (pendingKind, string, string, error) {
	if f, err := h.session.Queue.GetFile(queueID); err == nil {
		destPath := queue.UniquePath(filepath.Join(h.session.Config.ReceivedFilesDir, f.Name))
		if err := h.session.Transfer.ApproveFileTransfer(f, destPath); err != nil {
			return "", "", "", err
		}
		if err := h.session.Queue.RemoveFile(queueID); err != nil {
			return "", "", "", err
		}
		return pendingKindFile, destPath, f.Name, nil
	}

	if f, err := h.session.Queue.GetFolder(queueID); err == nil {
		destPath := queue.UniquePath(filepath.Join(h.session.Config.ReceivedFoldersDir, f.Name))
		if err := h.session.Transfer.ApproveFolderTransfer(f, destPath); err != nil {
			return "", "", "", err
		}
		if err := h.session.Queue.RemoveFolder(queueID); err != nil {
			return "", "", "", err
		}
		return pendingKindFolder, destPath, f.Name, nil
	}

	return "", "", "", queue.ErrQueueItemNotFound
}

func (h *Handler) rejectPendingByID(queueID int) (pendingKind, string, error) {
	if f, err := h.session.Queue.GetFile(queueID); err == nil {
		if err := h.session.Transfer.RejectFileTransfer(f); err != nil {
			return "", "", err
		}
		if err := h.session.Queue.RemoveFile(queueID); err != nil {
			return "", "", err
		}
		return pendingKindFile, f.Name, nil
	}

	if f, err := h.session.Queue.GetFolder(queueID); err == nil {
		if err := h.session.Transfer.RejectFolderTransfer(f); err != nil {
			return "", "", err
		}
		if err := h.session.Queue.RemoveFolder(queueID); err != nil {
			return "", "", err
		}
		return pendingKindFolder, f.Name, nil
	}

	return "", "", queue.ErrQueueItemNotFound
}

func (h *Handler) pendingSummaries() []pendingSummary {
	files := h.session.Queue.ListFiles()
	folders := h.session.Queue.ListFolders()

	items := make([]pendingSummary, 0, len(files)+len(folders))
	for _, f := range files {
		items = append(items, pendingSummary{
			ID:        f.ID,
			Kind:      pendingKindFile,
			Name:      f.Name,
			Sender:    f.Sender,
			SenderIP:  f.SenderIP,
			Size:      f.Size,
			Timestamp: f.Timestamp,
		})
	}
	for _, f := range folders {
		items = append(items, pendingSummary{
			ID:        f.ID,
			Kind:      pendingKindFolder,
			Name:      f.Name,
			Sender:    f.Sender,
			SenderIP:  f.SenderIP,
			Size:      f.Size,
			Timestamp: f.Timestamp,
		})
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].ID < items[j].ID
	})
	return items
}

func queuedLabel(ts time.Time) string {
	if ts.IsZero() {
		return "just now"
	}
	diff := time.Since(ts)
	if diff < 0 {
		diff = 0
	}
	switch {
	case diff < time.Minute:
		secs := int(diff.Round(time.Second) / time.Second)
		if secs <= 1 {
			return "1s ago"
		}
		return fmt.Sprintf("%ds ago", secs)
	case diff < time.Hour:
		mins := int(diff.Round(time.Minute) / time.Minute)
		if mins <= 1 {
			return "1m ago"
		}
		return fmt.Sprintf("%dm ago", mins)
	default:
		hours := int(diff.Round(time.Hour) / time.Hour)
		if hours <= 1 {
			return "1h ago"
		}
		return fmt.Sprintf("%dh ago", hours)
	}
}

func formatSize(size int64) string {
	return format.Size(size)
}
