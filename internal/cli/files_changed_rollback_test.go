package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
)

// files_changed MUST NOT REPORT THE TREE WHEN THE TREE WAS ROLLED BACK.
//
// THE INCIDENT, measured 2026-10-25. A scoped run (--allow two files, --stop-on
// off-scope-edit) wrote both of its allowed files, then attempted a third outside
// --allow. The stop rule ended the run; the rollback removed the two files the run
// had created. withFilesChanged then read the git working tree — which the rollback
// had just restored — and the JSON said:
//
//	"failure_code": "off_scope_edit", "files_changed": {"count": 0, "paths": []}
//
// The harness consuming that JSON recorded four `must_contain "…" not found` checks
// for strings the model had actually written, plus "no files[] content hash changed
// and no new: path was created", i.e. it diagnosed "the model never did the work".
// The ordering is the bug: writes → safety stop → rollback → accounting. By the time
// the accounting runs, the evidence has been deleted on purpose.
//
// The rollback stays exactly as it is. The accounting takes the run's OWN record of
// its writes (agent.Report.ChangedPaths, captured before the restore) and marks it
// rolled_back so a reader knows those paths are no longer on disk.
func TestFilesChangedSurvivesTheRollback(t *testing.T) {
	// A clean repo: the tree holds nothing, exactly as it does after a rollback.
	root := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-m", "seed")

	rep := &agent.Report{
		Reason:       agent.ReasonSafetyStop,
		Safety:       &agent.SafetyEvidence{Rule: "off-scope-edit", Class: "outside_allow", Tool: "edit_file", Path: "package.json"},
		RolledBack:   true,
		ChangedPaths: []string{"src/app/catalog/livery.spec.ts", "src/app/catalog/livery.ts"},
	}
	s := buildRunSummary(config.Config{Model: "m", BenchmarkMode: true}, "node --test", rep, time.Second, nil)
	withFilesChanged(&s, root, rep)

	if s.FilesChanged == nil {
		t.Fatal("files_changed should be populated in benchmark mode")
	}
	if s.FilesChanged.Count != 2 || len(s.FilesChanged.Paths) != 2 ||
		s.FilesChanged.Paths[0] != "src/app/catalog/livery.spec.ts" || s.FilesChanged.Paths[1] != "src/app/catalog/livery.ts" {
		t.Fatalf("files_changed = %+v, want the two files the run wrote — reading the restored tree "+
			"is what told the harness the model did nothing", s.FilesChanged)
	}
	if !s.FilesChanged.RolledBack {
		t.Fatal("files_changed.rolled_back must say these paths are NOT on disk any more")
	}
	// The flag has to be in the JSON, not only in the struct: the harness reads JSON.
	b, _ := json.Marshal(s.FilesChanged)
	if !json.Valid(b) || !strings.Contains(string(b), `"rolled_back":true`) {
		t.Fatalf("files_changed JSON = %s, want rolled_back:true", b)
	}
}

// TestFilesChangedReadsTheTreeWhenNothingWasRolledBack: the ordinary path is
// unchanged — a run whose work is still on disk is accounted from the git tree, and
// rolled_back is false so a reader can tell "these are on disk" from "these are gone".
func TestFilesChangedReadsTheTreeWhenNothingWasRolledBack(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	if err := os.WriteFile(filepath.Join(root, "base.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-m", "seed")
	if err := os.WriteFile(filepath.Join(root, "kept.txt"), []byte("the run's work, still here\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rep := &agent.Report{Reason: agent.ReasonSuccess, ChangedPaths: []string{"kept.txt"}}
	var s runSummary
	withFilesChanged(&s, root, rep)
	if s.FilesChanged == nil || s.FilesChanged.Count != 1 || s.FilesChanged.Paths[0] != "kept.txt" {
		t.Fatalf("files_changed = %+v, want [kept.txt] from the working tree", s.FilesChanged)
	}
	if s.FilesChanged.RolledBack {
		t.Fatal("nothing was rolled back; rolled_back must be false")
	}
}
