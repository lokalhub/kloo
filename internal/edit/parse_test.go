package edit

import (
	"errors"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		want  []Block
		isErr bool
	}{
		{
			name: "single block",
			in:   "internal/foo.go\n```go\n<<<<<<< SEARCH\nold line\n=======\nnew line\n>>>>>>> REPLACE\n```\n",
			// Bodies are line-anchored: they include the trailing newline before
			// the closing marker (decisions.md trailing-newline rule).
			want: []Block{{File: "internal/foo.go", Search: "old line\n", Replace: "new line\n"}},
		},
		{
			name: "multiple blocks same and different files",
			in: "a.go\n```\n<<<<<<< SEARCH\nA1\n=======\nA2\n>>>>>>> REPLACE\n```\n" +
				"a.go\n```\n<<<<<<< SEARCH\nB1\n=======\nB2\n>>>>>>> REPLACE\n```\n" +
				"b.go\n```\n<<<<<<< SEARCH\nC1\n=======\nC2\n>>>>>>> REPLACE\n```\n",
			want: []Block{
				{File: "a.go", Search: "A1\n", Replace: "A2\n"},
				{File: "a.go", Search: "B1\n", Replace: "B2\n"},
				{File: "b.go", Search: "C1\n", Replace: "C2\n"},
			},
		},
		{
			name: "surrounding prose tolerated",
			in: "Sure! Here is the change I propose.\n\nSome narration with a ```bash\necho hi\n``` ordinary fence.\n\n" +
				"main.go\n```go\n<<<<<<< SEARCH\nfmt.Println(\"a\")\n=======\nfmt.Println(\"b\")\n>>>>>>> REPLACE\n```\n\nThat should do it.\n",
			want: []Block{{File: "main.go", Search: "fmt.Println(\"a\")\n", Replace: "fmt.Println(\"b\")\n"}},
		},
		{
			name: "multi-line bodies preserved verbatim",
			in:   "x.go\n```\n<<<<<<< SEARCH\nline1\n  indented\nline3\n=======\nrepl1\nrepl2\n>>>>>>> REPLACE\n```\n",
			want: []Block{{File: "x.go", Search: "line1\n  indented\nline3\n", Replace: "repl1\nrepl2\n"}},
		},
		{
			name: "empty SEARCH is the new-file form",
			in:   "new.go\n```\n<<<<<<< SEARCH\n=======\npackage main\n>>>>>>> REPLACE\n```\n",
			want: []Block{{File: "new.go", Search: "", Replace: "package main\n"}},
		},
		{
			name:  "missing divider",
			in:    "a.go\n```\n<<<<<<< SEARCH\nold\nnew\n>>>>>>> REPLACE\n```\n",
			isErr: true,
		},
		{
			name:  "missing REPLACE terminator (unterminated)",
			in:    "a.go\n```\n<<<<<<< SEARCH\nold\n=======\nnew\n```\n",
			isErr: true,
		},
		{
			name:  "unclosed fence",
			in:    "a.go\n```\n<<<<<<< SEARCH\nold\n=======\nnew\n>>>>>>> REPLACE\n",
			isErr: true,
		},
		{
			name:  "missing filename line",
			in:    "```\n<<<<<<< SEARCH\nold\n=======\nnew\n>>>>>>> REPLACE\n```\n",
			isErr: true,
		},
		{
			name: "no blocks at all is not an error",
			in:   "just some prose, nothing to apply here.\n",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.in)
			if tc.isErr {
				if !errors.Is(err, ErrMalformedBlock) {
					t.Fatalf("want ErrMalformedBlock, got err=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d blocks, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("block %d mismatch\n got: %+v\nwant: %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
