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
	ID     string `json:"id"`
	Title  string `json:"title"`
	Model  string `json:"model"`
	Verify string `json:"verify"`
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
