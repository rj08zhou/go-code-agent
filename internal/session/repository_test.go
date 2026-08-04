package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateSessionID(t *testing.T) {
	for _, id := range []string{
		"20260730T120000-1234567",
		"session-1",
		"session_1",
		"session.1",
		"a",
	} {
		if err := ValidateSessionID(id); err != nil {
			t.Errorf("ValidateSessionID(%q): %v", id, err)
		}
	}

	for _, id := range []string{
		"",
		".",
		"..",
		"../outside",
		"session/child",
		`session\child`,
		"/absolute",
		"session with spaces",
		"会话",
		strings.Repeat("a", maxSessionIDBytes+1),
	} {
		err := ValidateSessionID(id)
		if !errors.Is(err, ErrInvalidSessionID) {
			t.Errorf("ValidateSessionID(%q) error = %v, want ErrInvalidSessionID", id, err)
		}
	}
}

func TestRepositoryRejectsSessionPathTraversal(t *testing.T) {
	dataDir := t.TempDir()
	repo := NewRepository(dataDir)

	if _, err := repo.SessionDir("../outside"); !errors.Is(err, ErrInvalidSessionID) {
		t.Fatalf("SessionDir traversal error = %v, want ErrInvalidSessionID", err)
	}
	if err := repo.CreateSession(&State{ID: "../outside"}); !errors.Is(err, ErrInvalidSessionID) {
		t.Fatalf("CreateSession traversal error = %v, want ErrInvalidSessionID", err)
	}
	if _, err := repo.LoadSessionMeta("../outside"); !errors.Is(err, ErrInvalidSessionID) {
		t.Fatalf("LoadSessionMeta traversal error = %v, want ErrInvalidSessionID", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "outside")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path traversal created data outside sessions root: %v", err)
	}
}

func TestLoadSessionMetaReportsDistinctFailures(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		repo := NewRepository(t.TempDir())
		_, err := repo.LoadSessionMeta("missing")
		if !errors.Is(err, ErrSessionNotFound) {
			t.Fatalf("error = %v, want ErrSessionNotFound", err)
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		repo := NewRepository(t.TempDir())
		dir, err := repo.SessionDir("broken")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = repo.LoadSessionMeta("broken")
		if !errors.Is(err, ErrInvalidSessionMetadata) {
			t.Fatalf("error = %v, want ErrInvalidSessionMetadata", err)
		}
	})

	t.Run("metadata ID mismatch", func(t *testing.T) {
		repo := NewRepository(t.TempDir())
		dir, err := repo.SessionDir("requested")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{"id":"different"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err = repo.LoadSessionMeta("requested")
		if !errors.Is(err, ErrInvalidSessionMetadata) {
			t.Fatalf("error = %v, want ErrInvalidSessionMetadata", err)
		}
	})
}

func TestSwitchActiveKeepsCurrentSessionWhenTargetMetadataIsBroken(t *testing.T) {
	repo := NewRepository(t.TempDir())
	current := &State{ID: "current", Title: "Current", Status: StatusActive}
	broken := &State{ID: "broken", Title: "Broken", Status: StatusActive}
	for _, st := range []*State{current, broken} {
		if err := repo.CreateSession(st); err != nil {
			t.Fatal(err)
		}
	}
	idx := &sessionsIndex{ActiveID: current.ID, Sessions: []State{*current, *broken}}
	if err := repo.SaveIndex(idx); err != nil {
		t.Fatal(err)
	}
	brokenDir, err := repo.SessionDir(broken.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brokenDir, "meta.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := repo.SwitchActive(broken.ID); !errors.Is(err, ErrInvalidSessionMetadata) {
		t.Fatalf("SwitchActive error = %v, want ErrInvalidSessionMetadata", err)
	}
	got, err := repo.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if got.ActiveID != current.ID {
		t.Fatalf("ActiveID = %q, want unchanged %q", got.ActiveID, current.ID)
	}
}

func TestSwitchActiveRejectsUnknownSession(t *testing.T) {
	repo := NewRepository(t.TempDir())
	if err := repo.SwitchActive("missing"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("SwitchActive error = %v, want ErrSessionNotFound", err)
	}
}

func TestRenameSessionUpdatesIndexTimestamp(t *testing.T) {
	repo := NewRepository(t.TempDir())
	st := &State{ID: "s1", Title: "One", Status: StatusActive}
	if err := repo.CreateSession(st); err != nil {
		t.Fatal(err)
	}
	meta, err := repo.LoadSessionMeta(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveIndex(&sessionsIndex{ActiveID: st.ID, Sessions: []State{*meta}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.RenameSession(st.ID, "Renamed"); err != nil {
		t.Fatal(err)
	}
	meta, err = repo.LoadSessionMeta(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := repo.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Sessions) != 1 || idx.Sessions[0].Title != "Renamed" {
		t.Fatalf("index session = %#v", idx.Sessions)
	}
	if idx.Sessions[0].UpdatedAt != meta.UpdatedAt {
		t.Fatalf("index UpdatedAt = %d, want meta %d", idx.Sessions[0].UpdatedAt, meta.UpdatedAt)
	}
}

func TestLoadIndexReturnsShortAndGeneratedSessionIDs(t *testing.T) {
	repo := NewRepository(t.TempDir())
	generated := NewSessionID()
	idx := &sessionsIndex{
		ActiveID: "a",
		Sessions: []State{
			{ID: "a", Status: StatusActive, Title: "Short"},
			{ID: generated, Status: StatusArchived, Title: "Generated"},
		},
	}
	if err := repo.SaveIndex(idx); err != nil {
		t.Fatal(err)
	}

	got, err := repo.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if got.ActiveID != "a" {
		t.Fatalf("ActiveID = %q, want %q", got.ActiveID, "a")
	}
	if len(got.Sessions) != 2 {
		t.Fatalf("session count = %d, want 2", len(got.Sessions))
	}
	if got.Sessions[0].ID != "a" || got.Sessions[0].Title != "Short" {
		t.Fatalf("first session = %#v", got.Sessions[0])
	}
	if got.Sessions[1].ID != generated || got.Sessions[1].Title != "Generated" {
		t.Fatalf("second session = %#v", got.Sessions[1])
	}
}

func TestListBackfillCandidatesSkipsActiveArchivedAndSaved(t *testing.T) {
	repo := NewRepository(t.TempDir())
	active := &State{ID: "active", Title: "Active", Status: StatusActive}
	saved := &State{ID: "saved", Title: "Saved", Status: StatusActive, MemorySaved: true}
	archived := &State{ID: "archived", Title: "Archived", Status: StatusArchived}
	pending := &State{ID: "pending", Title: "Pending", Status: StatusActive}
	for _, st := range []*State{active, saved, archived, pending} {
		if err := repo.CreateSession(st); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.SaveIndex(&sessionsIndex{
		ActiveID: active.ID,
		Sessions: []State{*active, *saved, *archived, *pending},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := repo.ListBackfillCandidates(active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != pending.ID {
		t.Fatalf("candidates = %#v, want only pending", got)
	}
}

func TestMarkMemorySavedUpdatesMetaAndIndex(t *testing.T) {
	repo := NewRepository(t.TempDir())
	st := &State{ID: "s1", Title: "One", Status: StatusActive}
	if err := repo.CreateSession(st); err != nil {
		t.Fatal(err)
	}
	if err := repo.SaveIndex(&sessionsIndex{ActiveID: st.ID, Sessions: []State{*st}}); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkMemorySaved(st.ID); err != nil {
		t.Fatal(err)
	}
	meta, err := repo.LoadSessionMeta(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !meta.MemorySaved {
		t.Fatal("meta MemorySaved not set")
	}
	idx, err := repo.LoadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Sessions) != 1 || !idx.Sessions[0].MemorySaved {
		t.Fatalf("index MemorySaved not set: %#v", idx.Sessions)
	}
}

func TestLoadIndexDoesNotHideCorruption(t *testing.T) {
	repo := NewRepository(t.TempDir())
	if err := os.WriteFile(repo.indexPath(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LoadIndex(); err == nil || !strings.Contains(err.Error(), "decode sessions index") {
		t.Fatalf("LoadIndex error = %v, want decode error", err)
	}
}
