package edit

import (
	"errors"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name    string
		content string
		block   Block
		want    MatchKind
	}{
		{
			name:    "new file (empty SEARCH)",
			content: "anything\n",
			block:   Block{Search: "", Replace: "fresh\n"},
			want:    MatchNewFile,
		},
		{
			name:    "search absent",
			content: "alpha\nbeta\n",
			block:   Block{Search: "gamma\n"},
			want:    MatchNotFound,
		},
		{
			name:    "search present once",
			content: "alpha\nbeta\n",
			block:   Block{Search: "beta\n"},
			want:    MatchUnique,
		},
		{
			name:    "search present twice",
			content: "x\nx\n",
			block:   Block{Search: "x\n"},
			want:    MatchAmbiguous,
		},
		{
			name:    "multi-line unique search",
			content: "func a() {\n\treturn 1\n}\n",
			block:   Block{Search: "func a() {\n\treturn 1\n}\n"},
			want:    MatchUnique,
		},
		{
			name:    "whitespace-different search is not fuzzy",
			content: "\treturn 1\n",                  // tab-indented
			block:   Block{Search: "    return 1\n"}, // space-indented
			want:    MatchNotFound,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.content, tc.block); got != tc.want {
				t.Errorf("Classify() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMatchKindString(t *testing.T) {
	cases := []struct {
		kind MatchKind
		want string
	}{
		{MatchNewFile, "new-file"},
		{MatchNotFound, "not-found"},
		{MatchUnique, "unique"},
		{MatchAmbiguous, "ambiguous"},
		{MatchKind(99), "unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.kind.String(); got != tc.want {
				t.Errorf("MatchKind(%d).String() = %q, want %q", tc.kind, got, tc.want)
			}
		})
	}
}

// TestClassifyAgreesWithApplyBlock is the consistency property: the classifier's
// verdict must match ApplyBlock's actual outcome on the same (content, block),
// because both must read from the same strings.Count semantics. If this ever
// fails, the diagnosis shown to the model has desynced from what an apply does.
func TestClassifyAgreesWithApplyBlock(t *testing.T) {
	cases := []struct {
		name    string
		content string
		block   Block
	}{
		{"unique", "alpha\nbeta\n", Block{Search: "beta\n", Replace: "BETA\n"}},
		{"not-found", "alpha\nbeta\n", Block{Search: "gamma\n", Replace: "g\n"}},
		{"ambiguous", "x\nx\n", Block{Search: "x\n", Replace: "y\n"}},
		{"new-file", "alpha\n", Block{Search: "", Replace: "new\n"}},
		{"multi-line unique", "a\nb\nc\n", Block{Search: "a\nb\n", Replace: "z\n"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind := Classify(tc.content, tc.block)
			_, err := ApplyBlock(tc.content, tc.block)
			switch kind {
			case MatchUnique:
				if err != nil {
					t.Errorf("Classify=unique but ApplyBlock returned err=%v", err)
				}
			case MatchNotFound:
				if !errors.Is(err, ErrSearchNotFound) {
					t.Errorf("Classify=not-found but ApplyBlock err=%v, want ErrSearchNotFound", err)
				}
			case MatchAmbiguous:
				if !errors.Is(err, ErrAmbiguousMatch) {
					t.Errorf("Classify=ambiguous but ApplyBlock err=%v, want ErrAmbiguousMatch", err)
				}
			case MatchNewFile:
				if !errors.Is(err, ErrEmptySearch) {
					t.Errorf("Classify=new-file but ApplyBlock err=%v, want ErrEmptySearch", err)
				}
			}
		})
	}
}
