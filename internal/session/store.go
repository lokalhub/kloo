// Package session persists a kloo conversation across runs AND across CLI
// invocations, so a follow-up ("what's the issue?", "continue") resumes with full
// context. Sessions are workspace-scoped: stored under
// {workspace}/.kloo/sessions/<id>.json, with the .kloo dir self-ignored from git
// (a generated .gitignore of "*") so transcripts — which can hold sensitive
// context — never get committed. The in-memory carry already exists (the TUI's
// session slice); this package is just the durable store + reload.
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/lokalhub/kloo/internal/llm"
)

// Session is one persisted conversation plus the metadata to list/resume it.
type Session struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Model string `json:"model"`
	// Provider and Endpoint travel WITH Model, because a model id is only meaningful
	// against the endpoint that serves it. Persisting Model alone is what made
	// restoring it unsafe: the saved id is the RESOLVED one (alias lookup is
	// provider-scoped), so a session recorded on openrouter stored
	// "deepseek/deepseek-v4-flash", and a resume that restored just the model sent
	// that id to whatever endpoint the default provider resolved to.
	//
	// omitempty ⇒ a session file written before this loads unchanged, with both
	// empty, which resume treats as "model not restorable" rather than guessing.
	Provider string `json:"provider,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	Verify   string `json:"verify"`
	// Lint is the resolved fast advisory lint command for this session, persisted
	// for resume parity beside Verify. omitempty ⇒ pre-existing session JSON without
	// a "lint" key loads unchanged (back-compat).
	Lint    string    `json:"lint,omitempty"`
	Runs    int       `json:"runs"` // completed runs (submissions) — the friendly "N runs" count
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
	// Messages is the MODEL-facing carry: one compact recap per run, re-seeded as
	// the loop's SessionHistory so follow-ups have context without a small model
	// parroting/replaying the raw transcript.
	Messages []llm.Message `json:"messages"`
	// Transcript is the HUMAN-facing display log: a compact, readable replay of the
	// conversation (your prompt, the assistant's prose, one-line tool summaries) so a
	// resumed session re-renders the prior turns. Deliberately NOT the raw run
	// transcript — file dumps and command output are summarised to a line, so one
	// JSON file stays small. omitempty ⇒ pre-existing sessions resume as before.
	Transcript []DisplayItem `json:"transcript,omitempty"`
}

// DisplayItem is one readable block of the resumable conversation. Kind is
// "user" (a prompt), "assistant" (the model's prose), or "tool" (a one-line action
// summary like "npm run build [exit 0]"); Text is the already-rendered, bounded
// content the TUI replays on resume.
type DisplayItem struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// Meta is the listing view (no messages) for the launch picker.
type Meta struct {
	ID      string
	Title   string
	Runs    int
	Updated time.Time
}

// Store is the on-disk session store for one workspace.
type Store struct{ dir string } // {workspace}/.kloo/sessions

// NewStore builds the store rooted at {workspace}/.kloo/sessions.
func NewStore(workspace string) *Store {
	return &Store{dir: filepath.Join(workspace, ".kloo", "sessions")}
}

// klooDir is the workspace .kloo directory (parent of sessions/).
func (s *Store) klooDir() string { return filepath.Dir(s.dir) }

// NewID mints a sortable, human-readable id from the clock.
func NewID(now time.Time) string { return now.Format("20060102-150405") }

// ValidID reports whether id is usable as a session id: non-empty, at most
// maxIDLen characters, and only letters, digits, '-' and '_'. The charset is
// deliberately narrow because the id IS the file name ({id}.json) — allowing
// '/', '.', or a space would let a rename escape the sessions directory or
// produce a file the listing can't round-trip.
func ValidID(id string) bool {
	if id == "" || len(id) > maxIDLen {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// maxIDLen bounds a session id so a rename can't mint a name longer than a
// filesystem comfortably takes (and so the exit hint stays one line).
const maxIDLen = 64

// Rename gives a session a new id, moving its file so the next launch can
// --resume the new name. It is the durable half of /rename: the TUI updates the
// in-memory session and calls this so the name survives a restart — which is the
// whole point, since a restarted session re-associates by id.
//
// Errors are returned, never swallowed: an invalid id, a collision with an
// existing session, or a failed write must leave the OLD file intact so the
// conversation is never lost to a rename.
func (s *Store) Rename(sess *Session, newID string) error {
	if sess == nil {
		return fmt.Errorf("session: rename: no session")
	}
	if !ValidID(newID) {
		return fmt.Errorf("session: rename: %q is not a valid id (letters, digits, '-' and '_' only, max %d)", newID, maxIDLen)
	}
	if newID == sess.ID {
		return nil // nothing to do — not an error
	}
	if _, err := os.Stat(filepath.Join(s.dir, newID+".json")); err == nil {
		return fmt.Errorf("session: rename: %q already exists", newID)
	}
	oldID := sess.ID
	sess.ID = newID
	if err := s.Save(sess); err != nil {
		sess.ID = oldID // keep the in-memory session pointing at the file that exists
		return err
	}
	// Remove the old file only AFTER the new one is safely written, so a crash
	// between the two leaves a duplicate rather than a lost conversation.
	if oldID != "" && oldID != newID {
		_ = os.Remove(filepath.Join(s.dir, oldID+".json"))
	}
	return nil
}

// Title derives a one-line title from the first task (bounded).
func Title(task string) string {
	t := strings.TrimSpace(strings.ReplaceAll(task, "\n", " "))
	if len(t) > 60 {
		t = t[:57] + "…"
	}
	return t
}

// List returns session metas, newest-updated first. A corrupt file is skipped, not
// fatal — a bad session must never block launching.
func (s *Store) List() ([]Meta, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var metas []Meta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		sess, err := s.load(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		metas = append(metas, Meta{ID: sess.ID, Title: sess.Title, Runs: sess.Runs, Updated: sess.Updated})
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].Updated.After(metas[j].Updated) })
	return metas, nil
}

// Load reads a session by id.
func (s *Store) Load(id string) (*Session, error) { return s.load(filepath.Join(s.dir, id+".json")) }

func (s *Store) load(path string) (*Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, fmt.Errorf("session: parse %s: %w", path, err)
	}
	return &sess, nil
}

// Save writes the session atomically, creating {workspace}/.kloo (self-ignored
// from git) and sessions/ on first use.
func (s *Store) Save(sess *Session) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	if err := ensureGitignore(s.klooDir()); err != nil {
		return err
	}
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return err
	}
	final := filepath.Join(s.dir, sess.ID+".json")
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final) // atomic replace
}

// ensureGitignore writes {workspace}/.kloo/.gitignore = "*" once, so session
// transcripts never land in the user's repo or a commit.
func ensureGitignore(klooDir string) error {
	p := filepath.Join(klooDir, ".gitignore")
	if _, err := os.Stat(p); err == nil {
		return nil // already present
	}
	return os.WriteFile(p, []byte("*\n"), 0o644)
}
