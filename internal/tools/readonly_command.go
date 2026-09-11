package tools

import "strings"

// The churn rail needs to tell "I ran the test suite to see where I am" from
// "I mutated the tree". Both arrive as run_command, and conflating them is
// fatal: after an edit, re-running the suite is the most natural thing an agent
// does, and three such turns against an unchanged failure used to halt the run.
// Measured on a real A/B, both 8k arms died on exactly that — steps 12, 15 and
// 17 were a bare `go test ./...`.
//
// Classifying an arbitrary shell command is undecidable, so this is a
// deliberately conservative allowlist: anything unrecognised counts as ACTING,
// which is the old behaviour. A miss costs a rail that fires slightly early; a
// false "read-only" would let a mutating loop run unchecked, so we never guess.

// readOnlyCommands are single-word commands that only inspect.
var readOnlyCommands = map[string]bool{
	"ls": true, "cat": true, "head": true, "tail": true, "wc": true,
	"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true,
	"find": true, "file": true, "stat": true, "pwd": true, "tree": true,
	"echo": true, "printf": true, "which": true, "type": true, "basename": true,
	"dirname": true, "date": true, "env": true, "printenv": true, "uname": true,
	"sort": true, "uniq": true, "cut": true, "diff": true, "cmp": true,
	"du": true, "df": true, "true": true, "false": true, "test": true,
	"pytest": true, "jq": true, "yq": true, "realpath": true, "readlink": true,
}

// readOnlyPrefixes are two-word forms whose FIRST word alone is ambiguous:
// `go test` inspects, `go generate` writes; `git diff` inspects, `git checkout`
// does not; `sed -n` prints, `sed -i` rewrites in place.
var readOnlyPrefixes = [][2]string{
	{"go", "test"}, {"go", "vet"}, {"go", "list"}, {"go", "doc"},
	{"go", "env"}, {"go", "version"},
	{"git", "status"}, {"git", "diff"}, {"git", "log"}, {"git", "show"},
	{"git", "branch"}, {"git", "ls-files"}, {"git", "rev-parse"}, {"git", "blame"},
	{"sed", "-n"}, {"gofmt", "-l"}, {"npm", "test"}, {"yarn", "test"},
	{"cargo", "test"}, {"cargo", "check"}, {"cargo", "clippy"},
	{"python", "-m"}, {"python3", "-m"},
}

// writeRedirects make any command mutating regardless of the program: `go test
// > out.txt` writes a file. `2>&1` and `2>/dev/null` are NOT file writes in the
// sense we care about, but distinguishing them costs more than it is worth, so
// only the plain forms below are screened and anything else falls through to
// "acting".
func hasWriteRedirect(seg string) bool {
	s := seg
	// Drop the stderr-merge and stderr-discard idioms models use constantly,
	// so `go test ./... 2>&1 | tail` still reads as diagnostic.
	for _, benign := range []string{"2>&1", "2>/dev/null", "2> /dev/null"} {
		s = strings.ReplaceAll(s, benign, "")
	}
	return strings.Contains(s, ">")
}

// IsReadOnlyCommand reports whether every stage of a shell command only
// inspects the tree. Pipelines and &&/||/; chains are split and each stage must
// independently qualify — one `rm` anywhere makes the whole thing acting.
func IsReadOnlyCommand(command string) bool {
	if strings.TrimSpace(command) == "" {
		return false
	}
	// Backticks and $(...) can hide anything; refuse to reason about them.
	if strings.Contains(command, "`") || strings.Contains(command, "$(") {
		return false
	}
	segments := splitShellSegments(command)
	if len(segments) == 0 {
		return false
	}
	for _, seg := range segments {
		if !segmentIsReadOnly(seg) {
			return false
		}
	}
	return true
}

// splitShellSegments breaks a command on the operators that separate distinct
// programs: | || && ; and newlines.
func splitShellSegments(command string) []string {
	repl := strings.NewReplacer("||", "\n", "&&", "\n", "|", "\n", ";", "\n")
	var out []string
	for _, seg := range strings.Split(repl.Replace(command), "\n") {
		if s := strings.TrimSpace(seg); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func segmentIsReadOnly(seg string) bool {
	if hasWriteRedirect(seg) {
		return false
	}
	fields := strings.Fields(seg)
	// Skip leading VAR=value assignments (`GOFLAGS=-mod=mod go test ./...`).
	for len(fields) > 0 && strings.Contains(fields[0], "=") && !strings.HasPrefix(fields[0], "-") {
		fields = fields[1:]
	}
	if len(fields) == 0 {
		return false
	}
	cmd := fields[0]
	if i := strings.LastIndex(cmd, "/"); i >= 0 {
		cmd = cmd[i+1:] // /usr/bin/go → go
	}
	if len(fields) >= 2 {
		for _, p := range readOnlyPrefixes {
			if cmd == p[0] && fields[1] == p[1] {
				return true
			}
		}
	}
	// A bare `go`/`git`/`sed`/`npm` with no recognised subcommand is ambiguous:
	// fall through to acting rather than guess.
	return readOnlyCommands[cmd]
}
