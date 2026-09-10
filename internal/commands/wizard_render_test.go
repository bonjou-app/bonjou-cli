package commands

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/bonjou-app/bonjou-cli/internal/network"
)

func TestWizardTeaFilterClampsSmallWindowSize(t *testing.T) {
	msg := wizardTeaFilter(nil, tea.WindowSizeMsg{Width: 0, Height: 2})

	size, ok := msg.(tea.WindowSizeMsg)
	if !ok {
		t.Fatalf("expected WindowSizeMsg, got %T", msg)
	}
	if size.Width != wizardMinWidth {
		t.Fatalf("width = %d, want %d", size.Width, wizardMinWidth)
	}
	if size.Height != wizardMinHeight {
		t.Fatalf("height = %d, want %d", size.Height, wizardMinHeight)
	}
}

func TestWizardTeaFilterKeepsUsableWindowSize(t *testing.T) {
	const width = 120
	const height = 40

	msg := wizardTeaFilter(nil, tea.WindowSizeMsg{Width: width, Height: height})

	size, ok := msg.(tea.WindowSizeMsg)
	if !ok {
		t.Fatalf("expected WindowSizeMsg, got %T", msg)
	}
	if size.Width != width || size.Height != height {
		t.Fatalf("size = %dx%d, want %dx%d", size.Width, size.Height, width, height)
	}
}

func TestWizardPeerLabelFitsRenderWidth(t *testing.T) {
	label := wizardPeerLabel(network.Peer{
		Username: "very-long-peer-name",
		IP:       "192.168.1.37",
	})

	if got, want := len([]rune(label)), wizardLabelWidth-2; got > want {
		t.Fatalf("label width = %d, want <= %d: %q", got, want, label)
	}
}
