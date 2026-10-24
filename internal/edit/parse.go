package edit

import (
	"fmt"
	"os"
	"strings"
)

// repairEnabled reports whether tolerant recovery of malformed model output is on
// (KLOO_TOOL_REPAIR=1) — the same switch that recovers truncated tool calls. Here
// it lets a structurally-complete SEARCH/REPLACE block through even when the model
// dropped the cosmetic closing ``` fence, instead of rejecting the edit and
// churning to a repetition_halt (observed on weaker models like GLM-4.5-air).
func repairEnabled() bool {
	v := os.Getenv("KLOO_TOOL_REPAIR")
	return v != "" && v != "0"
}

// Block is one parsed SEARCH/REPLACE edit: the target File, the exact Search
// text to locate, and the Replace text to substitute. An empty Search marks the
// new-file form (create File with Replace as its content).
type Block struct {
	File    string
	Search  string
	Replace string
}

// Parse extracts every well-formed SEARCH/REPLACE block from s, in source order.
//
// The recognised grammar (aider-style), with arbitrary prose allowed between
// blocks:
//
//	path/to/file.go
//	```            (optional language tag; ``` go is fine)
//	<<<<<<< SEARCH
//	…search text…
//	=======
//	…replace text…
//	>>>>>>> REPLACE
//	```
//
// The filename is the line directly above the opening fence. Search/Replace
// bodies are preserved verbatim (the exact lines between the markers, joined by
// "\n", with no implicit trailing newline added — see decisions.md). A broken
// marker sequence (missing divider/terminator, unclosed fence, or missing
// filename) returns an error wrapping ErrMalformedBlock, naming the defect — a
// block is never silently dropped or half-parsed.
func Parse(s string) ([]Block, error) {
	lines := strings.Split(s, "\n")
	var blocks []Block

	i := 0
	for i < len(lines) {
		// A block starts at an opening fence whose very next line is the SEARCH
		// marker. Any other fence (ordinary prose code block) is skipped.
		if !isOpenFence(lines[i]) || i+1 >= len(lines) || trim(lines[i+1]) != markerSearch {
			i++
			continue
		}

		fenceIdx := i
		searchIdx := i + 1

		// Filename: the line directly above the opening fence.
		if fenceIdx == 0 {
			return nil, fmt.Errorf("edit: SEARCH block has no filename line above the fence: %w", ErrMalformedBlock)
		}
		file := trim(lines[fenceIdx-1])
		if file == "" || isOpenFence(lines[fenceIdx-1]) || isMarker(file) {
			return nil, fmt.Errorf("edit: SEARCH block has no filename line above the fence: %w", ErrMalformedBlock)
		}

		// Divider: first "=======" after the SEARCH marker, before any other marker.
		dividerIdx := -1
		for j := searchIdx + 1; j < len(lines); j++ {
			t := trim(lines[j])
			if t == markerDivider {
				dividerIdx = j
				break
			}
			if t == markerReplace || t == markerSearch {
				break // a terminator/new block before the divider → malformed
			}
		}
		if dividerIdx == -1 {
			return nil, fmt.Errorf("edit: SEARCH without a %q divider: %w", markerDivider, ErrMalformedBlock)
		}

		// Terminator: first ">>>>>>> REPLACE" after the divider, before any other marker.
		replaceIdx := -1
		for j := dividerIdx + 1; j < len(lines); j++ {
			t := trim(lines[j])
			if t == markerReplace {
				replaceIdx = j
				break
			}
			if t == markerSearch || t == markerDivider {
				break
			}
		}
		if replaceIdx == -1 {
			return nil, fmt.Errorf("edit: block without a %q terminator: %w", markerReplace, ErrMalformedBlock)
		}

		// Closing fence must immediately follow the terminator — unless tolerant
		// recovery is on: the block is already structurally COMPLETE (SEARCH /
		// ======= / REPLACE all found), so a missing closing ``` fence is cosmetic.
		// #F: accept it instead of rejecting the edit and letting the model churn.
		hasFence := replaceIdx+1 < len(lines) && isCloseFence(lines[replaceIdx+1])
		if !hasFence && !repairEnabled() {
			return nil, fmt.Errorf("edit: block fence opened but never closed: %w", ErrMalformedBlock)
		}

		blocks = append(blocks, Block{
			File:    file,
			Search:  body(lines[searchIdx+1 : dividerIdx]),
			Replace: body(lines[dividerIdx+1 : replaceIdx]),
		})
		if hasFence {
			i = replaceIdx + 2
		} else {
			i = replaceIdx + 1
		}
	}

	return blocks, nil
}

// ParseFlexible parses SEARCH/REPLACE blocks accepting BOTH the fenced form
// (Parse) and bare, unfenced markers (parseBare). Small local models routinely
// omit the ``` fence; strict Parse then finds nothing and a caller that knows the
// target file (the edit_file tool) would otherwise silently no-op — the model
// thinks it edited, the file is untouched, and the run loops. A diff that opens a
// real but broken block still surfaces ErrMalformedBlock; only genuinely
// block-shaped input is accepted.
// A FENCED block with a broken marker sequence gets one lenient retry through
// parseBare, with the ``` fence lines stripped. parseBare already recovers the
// measured "SEARCH without a ======= divider" shape (the model writes
// ">>>>>>> REPLACE" where the divider belongs), but only for UNFENCED input:
// strict Parse rejected the same mistake first whenever the model ALSO wrapped it
// in a fence, so the identical defect was recoverable or fatal depending on a
// cosmetic ```. The retry can only succeed on block-shaped input — parseBare
// returns ErrMalformedBlock for anything it cannot read — so the strict error is
// still what surfaces when there is genuinely no block to recover.
//
// The retry is REFUSED when it would produce an empty REPLACE section. Without
// the ======= divider there is nothing in the text that says where the search
// lines stop and the replacement lines start, and parseBare resolves that by
// treating everything before ">>>>>>> REPLACE" as SEARCH. That reading is right
// for the measured shape (replacement lines follow the misplaced marker) and a
// pure GUESS when nothing follows it — the block is then read as "delete these
// lines", which is not a deletion the model asked for. A guess at a destructive
// intent is worse than the strict error, so that one keeps failing loudly.
func ParseFlexible(s string) ([]Block, error) {
	blocks, err := Parse(s)
	if err != nil {
		if recovered, rerr := parseBare(stripFences(s)); rerr == nil && len(recovered) > 0 && noEmptyReplace(recovered) {
			return recovered, nil
		}
		return nil, err
	}
	if len(blocks) > 0 {
		return blocks, nil
	}
	return parseBare(s)
}

// noEmptyReplace reports whether every block carries replacement text. See
// ParseFlexible: a recovered block with an empty REPLACE is an unresolvable guess,
// not a recovery.
func noEmptyReplace(blocks []Block) bool {
	for _, b := range blocks {
		if b.Replace == "" {
			return false
		}
	}
	return true
}

// stripFences drops every line that is nothing but a ``` fence (with or without a
// language tag), so a fenced block can be re-read by the unfenced parser without
// its closing fence landing inside a section body.
func stripFences(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if isOpenFence(l) {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// parseBare parses unfenced SEARCH/REPLACE blocks: the bare marker sequence with
// no surrounding ``` fence and no filename line (the caller supplies the path, so
// each Block's File is left empty for the tool to retarget). Malformed input that
// opens a SEARCH still errors — it is never silently dropped.
func parseBare(s string) ([]Block, error) {
	lines := strings.Split(s, "\n")
	var blocks []Block

	i := 0
	for i < len(lines) {
		if trim(lines[i]) != markerSearch {
			i++
			continue
		}
		searchIdx := i

		// Find the divider. Standard form: the first "=======" before any other
		// marker. LENIENT recovery: a weak model (e.g. gpt-oss) routinely OMITS the
		// "=======" and writes ">>>>>>> REPLACE" where the divider belongs, then the
		// replace body with no closing marker. If we reach a ">>>>>>> REPLACE" with no
		// "=======" seen, treat THAT as the divider — the two cases never collide
		// because a well-formed block always has "=======" before its REPLACE.
		dividerIdx, dividerIsReplace := -1, false
		for j := searchIdx + 1; j < len(lines); j++ {
			t := trim(lines[j])
			if t == markerDivider {
				dividerIdx = j
				break
			}
			if t == markerReplace {
				dividerIdx, dividerIsReplace = j, true
				break
			}
			if t == markerSearch {
				break // a new block opened before any divider → malformed
			}
		}
		if dividerIdx == -1 {
			return nil, fmt.Errorf("edit: SEARCH without a %q divider: %w", markerDivider, ErrMalformedBlock)
		}

		if dividerIsReplace {
			// ">>>>>>> REPLACE" was used as the divider, so there is no closing
			// marker: the replace body runs to the next SEARCH or EOF. Drop one
			// trailing empty line (the diff arg's trailing newline) so the body
			// matches the well-formed shape.
			replaceEnd := len(lines)
			for j := dividerIdx + 1; j < len(lines); j++ {
				if trim(lines[j]) == markerSearch {
					replaceEnd = j
					break
				}
			}
			rep := lines[dividerIdx+1 : replaceEnd]
			if len(rep) > 0 && rep[len(rep)-1] == "" {
				rep = rep[:len(rep)-1]
			}
			blocks = append(blocks, Block{
				Search:  body(lines[searchIdx+1 : dividerIdx]),
				Replace: body(rep),
			})
			i = replaceEnd
			continue
		}

		replaceIdx := -1
		for j := dividerIdx + 1; j < len(lines); j++ {
			t := trim(lines[j])
			if t == markerReplace {
				replaceIdx = j
				break
			}
			if t == markerSearch || t == markerDivider {
				break
			}
		}
		if replaceIdx == -1 {
			return nil, fmt.Errorf("edit: block without a %q terminator: %w", markerReplace, ErrMalformedBlock)
		}

		blocks = append(blocks, Block{
			Search:  body(lines[searchIdx+1 : dividerIdx]),
			Replace: body(lines[dividerIdx+1 : replaceIdx]),
		})
		i = replaceIdx + 1
	}

	return blocks, nil
}

func trim(s string) string { return strings.TrimSpace(s) }

// body reconstructs a SEARCH/REPLACE section body from its content lines,
// INCLUDING the trailing newline that precedes the closing marker line (each
// content line in the source is newline-terminated). An empty section (the
// new-file form) yields "". This is the trailing-newline rule (decisions.md):
// bodies are line-anchored, so created files end with a newline and edits match
// whole lines — the documented limitation is that editing a final line that
// lacks a trailing newline at EOF requires the SEARCH to omit it too.
func body(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// isOpenFence reports whether a line opens a code fence (``` optionally followed
// by a language tag).
func isOpenFence(line string) bool { return strings.HasPrefix(trim(line), "```") }

// isCloseFence reports whether a line is a bare closing fence.
func isCloseFence(line string) bool { return trim(line) == "```" }

// isMarker reports whether a trimmed line is one of the SEARCH/REPLACE markers
// (used to reject a marker masquerading as a filename).
func isMarker(trimmed string) bool {
	return trimmed == markerSearch || trimmed == markerDivider || trimmed == markerReplace
}
