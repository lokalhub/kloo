package edit

import "strings"

// MatchKind reports how a Block's SEARCH text relates to a target's content: the
// new-file form (empty SEARCH), absent, present exactly once, or present more
// than once. It is the diagnosis the harness shows the model when an apply fails;
// it is computed by Classify, the read-only twin of ApplyBlock.
type MatchKind int

const (
	// MatchNewFile is the empty-SEARCH form: the block creates File rather than
	// editing in place (ApplyBlock returns ErrEmptySearch on existing content).
	MatchNewFile MatchKind = iota
	// MatchNotFound: the SEARCH text is absent from content (strings.Count == 0);
	// ApplyBlock would return ErrSearchNotFound.
	MatchNotFound
	// MatchUnique: the SEARCH text occurs exactly once; ApplyBlock would apply.
	MatchUnique
	// MatchAmbiguous: the SEARCH text occurs more than once (strings.Count > 1);
	// ApplyBlock would return ErrAmbiguousMatch.
	MatchAmbiguous
)

// String returns a stable lowercase label for the kind, embedded verbatim in the
// model-facing repair observation (internal/agent) and asserted by tests, so the
// labels are a contract: "new-file", "not-found", "unique", "ambiguous".
func (k MatchKind) String() string {
	switch k {
	case MatchNewFile:
		return "new-file"
	case MatchNotFound:
		return "not-found"
	case MatchUnique:
		return "unique"
	case MatchAmbiguous:
		return "ambiguous"
	default:
		return "unknown"
	}
}

// Classify reports whether b's SEARCH text would apply against content, WITHOUT
// mutating anything or touching the filesystem. It is the single source of truth
// for "would this block apply", paired with ApplyBlock: it reuses the exact same
// strings.Count(content, b.Search) expression ApplyBlock switches on (apply.go),
// so the harness's diagnosis can never disagree with what an apply would do.
//
//   - b.Search == ""    ⇒ MatchNewFile (the empty-SEARCH new-file form; mirrors
//     ApplyBlock's ErrEmptySearch branch — no filesystem stat happens here, that
//     existence check stays in CreateFile/stageFile).
//   - count == 0         ⇒ MatchNotFound   (ApplyBlock → ErrSearchNotFound)
//   - count == 1         ⇒ MatchUnique     (ApplyBlock → applies)
//   - count > 1          ⇒ MatchAmbiguous  (ApplyBlock → ErrAmbiguousMatch)
func Classify(content string, b Block) MatchKind {
	if b.Search == "" {
		return MatchNewFile
	}
	switch strings.Count(content, b.Search) {
	case 0:
		return MatchNotFound
	case 1:
		return MatchUnique
	default:
		return MatchAmbiguous
	}
}
