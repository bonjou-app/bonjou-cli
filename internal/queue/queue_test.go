package queue

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManagerPersistsMetadataOnlyQueue(t *testing.T) {
	baseDir := t.TempDir()

	m, err := NewManager(baseDir)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	fileID, err := m.AddFile("req-file", "hamza", "192.168.1.3", "notes.txt", 1234, "")
	if err != nil {
		t.Fatalf("AddFile() error = %v", err)
	}
	folderID, err := m.AddFolder("req-folder", "abdulrehman", "192.168.1.8", "project", 9876, "a.txt\nb.txt")
	if err != nil {
		t.Fatalf("AddFolder() error = %v", err)
	}

	if err := m.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reloaded, err := NewManager(baseDir)
	if err != nil {
		t.Fatalf("NewManager() reload error = %v", err)
	}

	file, err := reloaded.GetFile(fileID)
	if err != nil {
		t.Fatalf("GetFile() error = %v", err)
	}
	if file.RequestID != "req-file" {
		t.Fatalf("file.RequestID = %q, want %q", file.RequestID, "req-file")
	}
	if file.Sender != "hamza" {
		t.Fatalf("file.Sender = %q, want %q", file.Sender, "hamza")
	}
	if file.SenderIP != "192.168.1.3" {
		t.Fatalf("file.SenderIP = %q, want %q", file.SenderIP, "192.168.1.3")
	}
	if file.Name != "notes.txt" {
		t.Fatalf("file.Name = %q, want %q", file.Name, "notes.txt")
	}
	if file.Size != 1234 {
		t.Fatalf("file.Size = %d, want %d", file.Size, 1234)
	}
	if file.Preview != "" {
		t.Fatalf("file.Preview = %q, want empty", file.Preview)
	}

	folder, err := reloaded.GetFolder(folderID)
	if err != nil {
		t.Fatalf("GetFolder() error = %v", err)
	}
	if folder.RequestID != "req-folder" {
		t.Fatalf("folder.RequestID = %q, want %q", folder.RequestID, "req-folder")
	}
	if folder.Sender != "abdulrehman" {
		t.Fatalf("folder.Sender = %q, want %q", folder.Sender, "abdulrehman")
	}
	if folder.SenderIP != "192.168.1.8" {
		t.Fatalf("folder.SenderIP = %q, want %q", folder.SenderIP, "192.168.1.8")
	}
	if folder.Name != "project" {
		t.Fatalf("folder.Name = %q, want %q", folder.Name, "project")
	}
	if folder.Size != 9876 {
		t.Fatalf("folder.Size = %d, want %d", folder.Size, 9876)
	}
	if folder.Preview != "a.txt\nb.txt" {
		t.Fatalf("folder.Preview = %q, want %q", folder.Preview, "a.txt\nb.txt")
	}

	nextID, err := reloaded.AddFile("req-next", "hamza", "192.168.1.3", "later.txt", 55, "")
	if err != nil {
		t.Fatalf("AddFile() after reload error = %v", err)
	}
	if nextID <= folderID {
		t.Fatalf("nextID = %d, want > %d", nextID, folderID)
	}
}

func TestManagerFlushClearsPersistedQueue(t *testing.T) {
	baseDir := t.TempDir()

	m, err := NewManager(baseDir)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	if _, err := m.AddFile("req-file", "hamza", "192.168.1.3", "notes.txt", 1234, ""); err != nil {
		t.Fatalf("AddFile() error = %v", err)
	}
	if _, err := m.AddFolder("req-folder", "abdulrehman", "192.168.1.8", "project", 9876, "manifest"); err != nil {
		t.Fatalf("AddFolder() error = %v", err)
	}

	if err := m.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}

	reloaded, err := NewManager(baseDir)
	if err != nil {
		t.Fatalf("NewManager() reload error = %v", err)
	}

	if got := len(reloaded.ListFiles()); got != 0 {
		t.Fatalf("len(ListFiles()) = %d, want 0", got)
	}
	if got := len(reloaded.ListFolders()); got != 0 {
		t.Fatalf("len(ListFolders()) = %d, want 0", got)
	}

	if _, err := reloaded.AddFile("req-reset", "hamza", "192.168.1.3", "reset.txt", 1, ""); err != nil {
		t.Fatalf("AddFile() after flush error = %v", err)
	} else if file, err := reloaded.GetFile(1); err != nil {
		t.Fatalf("GetFile(1) after flush error = %v", err)
	} else if file.Name != "reset.txt" {
		t.Fatalf("file.Name = %q, want %q", file.Name, "reset.txt")
	}
}

func TestManagerLoadDropsInvalidNamesAndKeepsSnapshotReadable(t *testing.T) {
	baseDir := t.TempDir()

	m, err := NewManager(baseDir)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	if _, err := m.AddFile("req-file", "hamza", "192.168.1.3", "safe.txt", 12, ""); err != nil {
		t.Fatalf("AddFile() error = %v", err)
	}

	snapshotPath := filepath.Join(baseDir, pendingDirName, snapshotFileName)
	if snapshotPath == "" {
		t.Fatal("snapshotPath should not be empty")
	}

	reloaded, err := NewManager(baseDir)
	if err != nil {
		t.Fatalf("NewManager() reload error = %v", err)
	}

	if got := len(reloaded.ListFiles()); got != 1 {
		t.Fatalf("len(ListFiles()) = %d, want 1", got)
	}
}

// A pending offer is live state. If the snapshot cannot be written the
// queue is no longer durable, but discarding the offer turns a recoverable
// disk problem into a lost transfer the user was never told about.
func TestAddKeepsItemWhenSnapshotWriteFails(t *testing.T) {
	base := t.TempDir()
	mgr, err := NewManager(base)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	// A regular file cannot contain a snapshot. This fails on Windows too,
	// where Chmod does not enforce Unix directory permissions, and as root.
	blocked := filepath.Join(base, "not-a-directory")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatalf("create blocking file: %v", err)
	}
	mgr.snapshotPath = filepath.Join(blocked, snapshotFileName)

	id, err := mgr.AddFile("req-1", "ada", "192.0.2.1", "important.pdf", 1024, "")
	if err != nil {
		t.Fatalf("AddFile returned an error when only persistence failed: %v", err)
	}
	if id == 0 {
		t.Fatal("AddFile returned no id")
	}

	item, err := mgr.GetFile(id)
	if err != nil {
		t.Fatalf("the offer was discarded when the snapshot write failed: %v", err)
	}
	if item.Name != "important.pdf" {
		t.Fatalf("item name = %q", item.Name)
	}
	if len(mgr.ListFiles()) != 1 {
		t.Fatalf("ListFiles = %d, want 1", len(mgr.ListFiles()))
	}

	// The failure is still reported, so a caller can say the queue will
	// not survive a restart.
	if mgr.PersistError() == nil {
		t.Error("PersistError is nil after a failed snapshot write")
	}
}

// Files and folders draw ids from one counter, so an id identifies
// exactly one pending item whichever kind it is.
func TestFileAndFolderIDsShareOneSequence(t *testing.T) {
	mgr, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	seen := map[int]bool{}
	for i := 0; i < 5; i++ {
		fileID, err := mgr.AddFile("f", "ada", "192.0.2.1", "file.txt", 10, "")
		if err != nil {
			t.Fatalf("AddFile: %v", err)
		}
		folderID, err := mgr.AddFolder("d", "ada", "192.0.2.1", "folder", 10, "")
		if err != nil {
			t.Fatalf("AddFolder: %v", err)
		}
		for _, id := range []int{fileID, folderID} {
			if seen[id] {
				t.Fatalf("id %d handed out twice", id)
			}
			seen[id] = true
		}
	}

	// Removing one kind must not disturb the other.
	files := mgr.ListFiles()
	if err := mgr.RemoveFile(files[0].ID); err != nil {
		t.Fatalf("RemoveFile: %v", err)
	}
	if got := len(mgr.ListFiles()); got != 4 {
		t.Fatalf("ListFiles after remove = %d, want 4", got)
	}
	if got := len(mgr.ListFolders()); got != 5 {
		t.Fatalf("ListFolders after removing a file = %d, want 5", got)
	}
}
