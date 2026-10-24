package edit

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestEmptySearchAppendsToExistingFile locks in the APPEND form.
//
// Measured defect (qwen3.8-flash-next, effort medium): a run asked to ADD two new
// public items to an existing Rust file died with run stopped — CHURN,
// failed_edits=3, zero files changed, after 17m11s. When the task is to append,
// there is nothing to SEARCH for, and the engine offered no shape that expresses
// "add this at the end": the empty-SEARCH form meant create-or-ErrFileExists.
//
// The rule is now uniform and teachable: EMPTY SEARCH = add Replace at the end,
// creating the file if it does not exist. Appending is strictly additive, so it
// cannot destroy content the model never read.
func TestEmptySearchAppendsToExistingFile(t *testing.T) {
	cases := []struct {
		name    string
		initial string
		replace string
		want    string
	}{
		{
			name:    "appends after a trailing newline",
			initial: "fn a() {}\n",
			replace: "pub fn b() {}\n",
			want:    "fn a() {}\npub fn b() {}\n",
		},
		{
			name:    "inserts the missing newline boundary",
			initial: "fn a() {}",
			replace: "pub fn b() {}\n",
			want:    "fn a() {}\npub fn b() {}\n",
		},
		{
			name:    "appends to an empty file without a leading blank line",
			initial: "",
			replace: "pub fn b() {}\n",
			want:    "pub fn b() {}\n",
		},
	}

	for _, tc := range cases {
		t.Run("ApplyToFile/"+tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lib.rs")
			if err := os.WriteFile(path, []byte(tc.initial), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := ApplyToFile(path, Block{Search: "", Replace: tc.replace}); err != nil {
				t.Fatalf("append must succeed on an existing file, got %v", err)
			}
			got, _ := os.ReadFile(path)
			if string(got) != tc.want {
				t.Errorf("content = %q, want %q", got, tc.want)
			}
		})

		t.Run("Apply/"+tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "lib.rs")
			if err := os.WriteFile(path, []byte(tc.initial), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Apply([]Block{{File: path, Search: "", Replace: tc.replace}}); err != nil {
				t.Fatalf("append must succeed through the batch path, got %v", err)
			}
			got, _ := os.ReadFile(path)
			if string(got) != tc.want {
				t.Errorf("content = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestEmptySearchStillCreatesMissingFile pins the unchanged half of the rule: on a
// file that does not exist, empty SEARCH still CREATES it with Replace verbatim.
func TestEmptySearchStillCreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "new.rs")
	if err := ApplyToFile(path, Block{Search: "", Replace: "pub fn b() {}\n"}); err != nil {
		t.Fatalf("create must still work, got %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "pub fn b() {}\n" {
		t.Errorf("content = %q", got)
	}
}

// TestAppendRefusesDuplicateContent is the guard that makes the append form safe to
// retry: a model that re-sends an append it already landed (the churn shape this
// whole change exists to stop) must be TOLD the content is already there, not
// quietly handed a second copy.
func TestAppendRefusesDuplicateContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lib.rs")
	if err := os.WriteFile(path, []byte("fn a() {}\npub fn b() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := ApplyToFile(path, Block{Search: "", Replace: "pub fn b() {}\n"})
	if !errors.Is(err, ErrAlreadyPresent) {
		t.Fatalf("want ErrAlreadyPresent, got %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "fn a() {}\npub fn b() {}\n" {
		t.Errorf("file must be byte-identical after a refused append, got %q", got)
	}

	if _, err := Apply([]Block{{File: path, Search: "", Replace: "pub fn b() {}\n"}}); !errors.Is(err, ErrAlreadyPresent) {
		t.Fatalf("batch path: want ErrAlreadyPresent, got %v", err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != "fn a() {}\npub fn b() {}\n" {
		t.Errorf("batch path must leave the file byte-identical, got %q", got)
	}
}

// TestParseFlexibleRecoversFencedMissingDivider covers the second measured shape:
// "SEARCH without a \"=======\" divider". parseBare already recovers a model that
// writes ">>>>>>> REPLACE" where the divider belongs, but only for UNFENCED input —
// strict Parse errors out first when the model also wrapped it in a ``` fence, and
// ParseFlexible returned that error without ever trying the lenient path.
func TestParseFlexibleRecoversFencedMissingDivider(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantSearch  string
		wantReplace string
	}{
		{
			name:        "fenced, REPLACE used as the divider",
			in:          "lib.rs\n```rust\n<<<<<<< SEARCH\nfn a() {}\n>>>>>>> REPLACE\nfn a() { b() }\n```\n",
			wantSearch:  "fn a() {}\n",
			wantReplace: "fn a() { b() }\n",
		},
		{
			name:        "unfenced, REPLACE used as the divider (already worked)",
			in:          "<<<<<<< SEARCH\nfn a() {}\n>>>>>>> REPLACE\nfn a() { b() }\n",
			wantSearch:  "fn a() {}\n",
			wantReplace: "fn a() { b() }\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			blocks, err := ParseFlexible(tc.in)
			if err != nil {
				t.Fatalf("ParseFlexible must recover this shape, got %v", err)
			}
			if len(blocks) != 1 {
				t.Fatalf("want 1 block, got %d", len(blocks))
			}
			if blocks[0].Search != tc.wantSearch || blocks[0].Replace != tc.wantReplace {
				t.Errorf("search=%q replace=%q, want %q / %q",
					blocks[0].Search, blocks[0].Replace, tc.wantSearch, tc.wantReplace)
			}
		})
	}
}

// TestParseFlexibleStillRejectsNonBlocks proves the lenient retry did not turn the
// parser into a yes-man: prose, and a SEARCH with no second marker at all, must
// still fail loudly rather than be invented into an edit.
func TestParseFlexibleStillRejectsNonBlocks(t *testing.T) {
	t.Run("prose yields no blocks and no error", func(t *testing.T) {
		blocks, err := ParseFlexible("please change hello to goodbye\n")
		if err != nil || len(blocks) != 0 {
			t.Errorf("want 0 blocks and nil error, got %d / %v", len(blocks), err)
		}
	})
	t.Run("SEARCH with no divider and no terminator still errors", func(t *testing.T) {
		if _, err := ParseFlexible("```\n<<<<<<< SEARCH\nfn a() {}\n```\n"); !errors.Is(err, ErrMalformedBlock) {
			t.Errorf("want ErrMalformedBlock, got %v", err)
		}
	})
	// The lenient retry must not GUESS a deletion. With no ======= divider and
	// nothing after ">>>>>>> REPLACE", nothing in the text says where the search
	// lines end, and reading them all as SEARCH with an empty REPLACE would turn a
	// malformed edit into a delete the model never asked for.
	t.Run("fenced missing divider with no replacement text still errors", func(t *testing.T) {
		if _, err := ParseFlexible("lib.rs\n```\n<<<<<<< SEARCH\nfn a() {}\n>>>>>>> REPLACE\n```\n"); !errors.Is(err, ErrMalformedBlock) {
			t.Errorf("want ErrMalformedBlock, got %v", err)
		}
	})
}
