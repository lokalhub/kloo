package agent

import "testing"

func TestNoOpSigBanDefaultsOff(t *testing.T) {
	if noOpSigBan() {
		t.Fatal("KLOO_NOOP_SIG_BAN must default OFF")
	}
	t.Setenv("KLOO_NOOP_SIG_BAN", "1")
	if !noOpSigBan() {
		t.Fatal("KLOO_NOOP_SIG_BAN=1 must enable the ban")
	}
}

// The ban is keyed on the NORMALISED signature, so a model that resends the same
// edit with different whitespace is still refused — otherwise the constraint is
// one reformat away from useless.
func TestNoOpSigBanIgnoresWhitespace(t *testing.T) {
	a := normalizeChurn("edit_file src/a.ts\n<<<<<<< SEARCH\nfoo   bar\n")
	b := normalizeChurn("edit_file src/a.ts\n<<<<<<< SEARCH\nfoo bar\n")
	if a != b {
		t.Fatalf("whitespace must not defeat the ban:\n%q\n%q", a, b)
	}
}

// Two DIFFERENT edits to the same file must not collide: banning by file was
// measured on A16 and lost the case, because the file the model was stuck on was
// also the file still missing the fix.
func TestNoOpSigBanIsPerEditNotPerFile(t *testing.T) {
	one := normalizeChurn("edit_file src/a.ts\n<<<<<<< SEARCH\nSELECT x\n")
	two := normalizeChurn("edit_file src/a.ts\n<<<<<<< SEARCH\nLEFT JOIN y\n")
	if one == two {
		t.Fatal("distinct edits to one file must have distinct signatures")
	}
}
