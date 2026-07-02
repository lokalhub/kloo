package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeManifest writes .kloo/scope.yaml under a fresh temp dir and returns the dir.
func writeManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".kloo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ScopeManifestFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolveScopeManifestBlockList(t *testing.T) {
	dir := writeManifest(t, `# scope for the benchmark
allow:
  - "src/**"
  - lib/**
deny:
  - .env
  - "dist/**"
read_only:
  - tests/**
`)
	got, err := ResolveScope(ScopeFlags{}, dir)
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}
	want := ScopeConfig{
		Allow:    []string{"src/**", "lib/**"},
		Deny:     []string{".env", "dist/**"},
		ReadOnly: []string{"tests/**"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("manifest parse:\n got %+v\nwant %+v", got, want)
	}
}

func TestResolveScopeManifestInlineList(t *testing.T) {
	dir := writeManifest(t, `allow: ["src/**", cmd/**]
deny: [".env"]
`)
	got, err := ResolveScope(ScopeFlags{}, dir)
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}
	if !reflect.DeepEqual(got.Allow, []string{"src/**", "cmd/**"}) {
		t.Fatalf("inline allow = %v", got.Allow)
	}
	if !reflect.DeepEqual(got.Deny, []string{".env"}) {
		t.Fatalf("inline deny = %v", got.Deny)
	}
}

// TestResolveScopeFlagsOverrideManifestKeyByKey: a non-nil flag list REPLACES that
// key's manifest list; keys with no flag keep the manifest value (not appended).
func TestResolveScopeFlagsOverrideManifestKeyByKey(t *testing.T) {
	dir := writeManifest(t, `allow:
  - manifest/**
deny:
  - manifest-deny/**
read_only:
  - manifest-ro/**
`)
	got, err := ResolveScope(ScopeFlags{
		Allow: []string{"flag/**"}, // replaces manifest allow
		// Deny not set (nil) → manifest deny stands
		ReadOnly: []string{}, // set-but-empty → clears manifest read_only
	}, dir)
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}
	if !reflect.DeepEqual(got.Allow, []string{"flag/**"}) {
		t.Fatalf("allow override = %v, want [flag/**]", got.Allow)
	}
	if !reflect.DeepEqual(got.Deny, []string{"manifest-deny/**"}) {
		t.Fatalf("deny (unset flag) = %v, want manifest value", got.Deny)
	}
	if len(got.ReadOnly) != 0 {
		t.Fatalf("read_only (empty flag) = %v, want empty", got.ReadOnly)
	}
}

// TestResolveScopeCommaAndRepeatableEquivalent: cobra splits commas for
// StringSlice, so ["a","b"] (repeatable) and the comma form yield the same list;
// blanks are dropped and order preserved.
func TestResolveScopeCommaAndRepeatableEquivalent(t *testing.T) {
	dir := t.TempDir() // no manifest
	repeatable, err := ResolveScope(ScopeFlags{Allow: []string{"src/**", "cmd/**"}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	commaSplit, err := ResolveScope(ScopeFlags{Allow: []string{"src/**", "cmd/**", ""}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(repeatable.Allow, commaSplit.Allow) {
		t.Fatalf("normalized lists differ: %v vs %v", repeatable.Allow, commaSplit.Allow)
	}
	if !reflect.DeepEqual(repeatable.Allow, []string{"src/**", "cmd/**"}) {
		t.Fatalf("normalized = %v", repeatable.Allow)
	}
}

func TestResolveScopeMissingManifestIsNotError(t *testing.T) {
	got, err := ResolveScope(ScopeFlags{}, t.TempDir())
	if err != nil {
		t.Fatalf("missing manifest must not error: %v", err)
	}
	if got.Active() {
		t.Fatalf("no manifest + no flags must be inactive, got %+v", got)
	}
}

func TestResolveScopeMalformedManifest(t *testing.T) {
	dir := writeManifest(t, "allow: {not a list}\n")
	if _, err := ResolveScope(ScopeFlags{}, dir); err == nil {
		t.Fatal("expected an error for a malformed manifest value")
	}
}

func TestParseStopOn(t *testing.T) {
	cases := []struct {
		name    string
		rules   []string
		want    StopPolicy
		wantErr bool
	}{
		{name: "off-scope-edit", rules: []string{"off-scope-edit"}, want: StopPolicy{OffScopeEdit: true}},
		{name: "read-only-edit", rules: []string{"read-only-edit"}, want: StopPolicy{ReadOnlyEdit: true}},
		{name: "repeated-verify=2", rules: []string{"repeated-verify=2"}, want: StopPolicy{RepeatedVerify: 2}},
		{name: "colon separator", rules: []string{"repeated-verify:3"}, want: StopPolicy{RepeatedVerify: 3}},
		{name: "combination", rules: []string{"off-scope-edit", "repeated-verify=3", "read-only-edit"}, want: StopPolicy{OffScopeEdit: true, ReadOnlyEdit: true, RepeatedVerify: 3}},
		{name: "blank ignored", rules: []string{"", "off-scope-edit", "  "}, want: StopPolicy{OffScopeEdit: true}},
		{name: "unknown rule", rules: []string{"blow-up-everything"}, wantErr: true},
		{name: "repeated-verify no value", rules: []string{"repeated-verify"}, wantErr: true},
		{name: "repeated-verify zero", rules: []string{"repeated-verify=0"}, wantErr: true},
		{name: "repeated-verify negative", rules: []string{"repeated-verify=-1"}, wantErr: true},
		{name: "repeated-verify NaN", rules: []string{"repeated-verify=lots"}, wantErr: true},
		{name: "off-scope-edit with value", rules: []string{"off-scope-edit=1"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseStopOn(tc.rules)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseStopOn(%v) = %+v, want error", tc.rules, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseStopOn(%v): %v", tc.rules, err)
			}
			if got != tc.want {
				t.Fatalf("parseStopOn(%v) = %+v, want %+v", tc.rules, got, tc.want)
			}
		})
	}
}

// TestResolveStopOnConfigError: an invalid --stop-on rule surfaces from Resolve as
// a "config:" error (the CLI maps it to failure_code config_error).
func TestResolveStopOnConfigError(t *testing.T) {
	_, err := Resolve(Flags{StopOn: []string{"bogus-rule"}}, func(string) string { return "" }, "")
	if err == nil {
		t.Fatal("expected a config error for an invalid --stop-on rule")
	}
}

// TestResolvePatchOnlyAndScopeFlags: PatchOnly + raw scope-flag passthrough land on
// the Config for the CLI to build the policy from.
func TestResolvePatchOnlyAndScopeFlags(t *testing.T) {
	patch := true
	cfg, err := Resolve(Flags{
		PatchOnly:  &patch,
		ScopeAllow: []string{"src/**"},
		StopOn:     []string{"off-scope-edit", "repeated-verify=2"},
	}, func(string) string { return "" }, "")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PatchOnly {
		t.Fatal("PatchOnly should be true")
	}
	if !reflect.DeepEqual(cfg.ScopeAllow, []string{"src/**"}) {
		t.Fatalf("ScopeAllow = %v", cfg.ScopeAllow)
	}
	if !cfg.StopOn.OffScopeEdit || cfg.StopOn.RepeatedVerify != 2 {
		t.Fatalf("StopOn = %+v", cfg.StopOn)
	}
}
