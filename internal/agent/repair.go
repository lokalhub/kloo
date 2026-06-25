package agent

import (
	"fmt"
	"strings"

	"github.com/lokalhub/kloo/internal/edit"
	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/tools"
)

// buildRepairObservation renders the enriched, model-facing message shown when an
// edit_file SEARCH/REPLACE block fails to apply because its SEARCH text did not
// match (or was ambiguous). It names the failing block(s) and the reason, shows
// the file's ACTUAL current contents (re-read through the jail, 5 MiB-capped),
// and instructs the model to re-issue the edit against the real text.
//
// It is pure string assembly plus one jailed read — it holds no loop state and
// mutates nothing. It returns (msg, false) on any condition under which the
// caller should fall back to the bare error observation:
//   - the target can't be read whole (oversize, missing, jail escape),
//   - the diff doesn't parse into any block (the bare ErrMalformedBlock message
//     already tells the model to fix the block shape),
//   - no parsed block actually fails to match (shouldn't happen on this path).
//
// Matching is diagnosed with edit.Classify (the read-only twin of ApplyBlock), so
// the reasons shown can never disagree with what an apply would do; the diff is
// re-parsed with edit.ParseFlexible — the same parser the edit_file tool uses —
// so there is no forked grammar.
func buildRepairObservation(root, path, diff string) (llm.Message, bool) {
	// 1. Re-read the target's ACTUAL content through the jail with the read_file
	//    5 MiB cap. Any failure (oversize, missing, jail escape) ⇒ fall back to the
	//    bare observation rather than read unbounded or guess at content.
	ws, err := tools.NewWorkspace(root)
	if err != nil {
		return llm.Message{}, false
	}
	content, err := tools.ReadFile(ws, path)
	if err != nil {
		return llm.Message{}, false
	}

	// 1a. Empty target: there is NO text for a SEARCH block to match, so the generic
	//     "make your SEARCH match the contents exactly" instruction below is
	//     unsatisfiable — and a weak model loops re-reading the empty file trying to
	//     find something to match (the canonical flail, [[kloo-edit-silent-noop]]).
	//     Tell it the file is empty and to use write_file instead, before rendering
	//     the (impossible) match instruction.
	if strings.TrimSpace(content) == "" {
		msg := fmt.Sprintf("tool edit_file could not apply to %s: the file is EMPTY (0 meaningful bytes), "+
			"so there is no text for a SEARCH block to match. Do NOT use edit_file SEARCH/REPLACE on an "+
			"empty file — call write_file with the full intended contents of %s instead.\n", path, path)
		return llm.Message{Role: llm.RoleUser, Content: msg}, true
	}

	// 2. Re-parse the model's diff with the SAME parser the tool uses (no fork).
	blocks, err := edit.ParseFlexible(diff)
	if err != nil || len(blocks) == 0 {
		return llm.Message{}, false
	}

	// 3. Classify each block; keep the ones that actually fail to match.
	type failing struct {
		block edit.Block
		kind  edit.MatchKind
	}
	var fails []failing
	for _, b := range blocks {
		switch k := edit.Classify(content, b); k {
		case edit.MatchNotFound, edit.MatchAmbiguous:
			fails = append(fails, failing{block: b, kind: k})
		}
	}
	if len(fails) == 0 {
		return llm.Message{}, false
	}

	// 4. Render the canonical observation (overview §3.3).
	var b strings.Builder
	fmt.Fprintf(&b, "tool edit_file could not apply to %s: the SEARCH text did not match the file.\n", path)

	anyAmbiguous := false
	for i, f := range fails {
		if f.kind == edit.MatchAmbiguous {
			anyAmbiguous = true
		}
		fmt.Fprintf(&b, "\nFailing SEARCH block #%d — %s:\n", i+1, failReason(f.kind))
		b.WriteString(renderBlock(f.block))
	}

	fmt.Fprintf(&b, "\nActual current contents of %s (re-read from disk; capped at the read_file 5 MiB limit):\n", path)
	fmt.Fprintf(&b, "----- BEGIN %s -----\n", path)
	b.WriteString(content)
	if !strings.HasSuffix(content, "\n") {
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "----- END %s -----\n", path)

	b.WriteString("\nFix this edit: re-issue edit_file with a SEARCH block whose text matches the actual " +
		"contents above exactly — byte-for-byte, including indentation and blank lines. Do not invent " +
		"lines that are not present.")
	if anyAmbiguous {
		b.WriteString(" For an ambiguous block, make the SEARCH more specific (include surrounding " +
			"lines) so it matches exactly once.")
	}
	b.WriteString("\n")

	return llm.Message{Role: llm.RoleUser, Content: b.String()}, true
}

// failReason maps a failing MatchKind to the human-readable reason embedded in
// the observation. The MatchKind.String() label ("not-found"/"ambiguous") is kept
// verbatim so the text stays a stable, asserted contract.
func failReason(k edit.MatchKind) string {
	switch k {
	case edit.MatchAmbiguous:
		return "ambiguous (the SEARCH text matches more than once)"
	default: // MatchNotFound
		return "not-found (the SEARCH text is not present in the file)"
	}
}

// renderBlock reproduces a parsed block as the canonical fenced-less SEARCH/
// REPLACE marker form so the model sees exactly which block it sent. The block's
// Search/Replace bodies already carry their trailing newline (edit.body), so the
// markers land on their own lines.
func renderBlock(b edit.Block) string {
	var sb strings.Builder
	sb.WriteString("<<<<<<< SEARCH\n")
	sb.WriteString(b.Search)
	sb.WriteString("=======\n")
	sb.WriteString(b.Replace)
	sb.WriteString(">>>>>>> REPLACE\n")
	return sb.String()
}
