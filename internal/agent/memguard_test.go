package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/repomap"
)

// NOTHING IN THIS FILE ALLOCATES MEMORY TO EXERCISE THE CEILING. The parser reads a
// /proc-shaped fixture and the budget is tested against known file sizes, so the
// guard is proved at a few kilobytes. A test that had to allocate 8 GiB to show the
// ceiling works is a test nobody runs, on a machine that has already had one
// unplanned restart.

func TestProcessRSSParsesProcStatus(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		body string
		want uint64
		ok   bool
	}{
		{"kB", "Name:\tkloo\nVmPeak:\t  120000 kB\nVmRSS:\t   18180 kB\nThreads:\t9\n", 18180 * 1024, true},
		{"first line", "VmRSS:\t       4 kB\n", 4 * 1024, true},
		// A unit kloo has never seen must not be read as a 1024x smaller number,
		// which would present itself as "plenty of room".
		{"mB", "VmRSS:\t   2048 mB\n", 2048 * 1024 * 1024, true},
		{"no VmRSS", "Name:\tkloo\nThreads:\t9\n", 0, false},
		{"garbage", "VmRSS:\tnot-a-number kB\n", 0, false},
		{"truncated", "VmRSS:\n", 0, false},
	}
	for _, tc := range cases {
		p := filepath.Join(dir, tc.name)
		if err := os.WriteFile(p, []byte(tc.body), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := rssFrom(p)
		if tc.ok && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: want an error so the guard goes inert, got %d", tc.name, got)
		}
		if got != tc.want {
			t.Errorf("%s: rss = %d, want %d", tc.name, got, tc.want)
		}
	}
	// A missing file must make the guard INERT, never fail a run: the alternative is
	// a diagnostic that refuses to run the agent because it cannot find a proc file.
	if _, err := rssFrom(filepath.Join(dir, "absent")); err == nil {
		t.Error("a missing /proc/self/status must report an error, not a zero reading")
	}
}

// TestMemCeilingSignConvention pins kloo's convention: 0 ⇒ computed default,
// negative ⇒ disabled, positive ⇒ verbatim. The word forms matter because someone
// reaching for this in the field types `=off` before `=-1`.
func TestMemCeilingSignConvention(t *testing.T) {
	cases := []struct {
		env  string
		want int64 // -1 means "the computed default, whatever it is"
	}{
		{"", -1},
		{"0", -1},
		{"-1", 0},
		{"off", 0},
		{"disabled", 0},
		{"2048", 2048 << 20},
		{"64", 64 << 20}, // the smallest value that can mean a real intent
		{"nonsense", -1}, // unreadable ⇒ the default, never a silent disable
		// A QUANTITY knob must not inherit envTri's boolean words. envTri maps
		// on/true/yes/always to 1, and reading that as 1 MiB gave a ceiling every live
		// process is already over — so KLOO_MEM_CEILING_MB=on aborted every run at step
		// one. `=on` is a very plausible thing to type at a guard that is already on by
		// default, so it must mean "use the default", not "fail everything".
		{"on", -1},
		{"true", -1},
		{"yes", -1},
		{"always", -1},
		{"default", -1},
		// Likewise a positive number too small to be anyone's intent: it can only break
		// every run, so it degrades to the default rather than bricking the tool.
		{"1", -1},
		{"63", -1},
	}
	for _, tc := range cases {
		t.Setenv(EnvMemCeilingMB, tc.env)
		got := memCeilingBytes()
		want := tc.want
		if want == -1 {
			want = int64(defaultMemCeilingMB(totalRAMBytes())) << 20
		}
		if got != want {
			t.Errorf("%s=%q ⇒ %d, want %d", EnvMemCeilingMB, tc.env, got, want)
		}
	}
	// And the guard is ON by default — this codebase has shipped comments claiming
	// "off by default" for rails that were on.
	t.Setenv(EnvMemCeilingMB, "")
	if memCeilingBytes() <= 0 {
		t.Fatal("the memory ceiling must be ENABLED by default")
	}
	if src := MemCeilingSource(); src != "default" {
		t.Errorf("unset ⇒ source %q, want default", src)
	}
}

// TestDefaultMemCeilingTracksTheBudgetTheProcessIsUnder covers the deliberate shape
// of the default: a share of available memory, capped, floored. A flat constant would
// be memory-blind in the way workingset.go's flat constant was window-blind — on a
// small container it never fires before the kernel does, which is where being killed
// hurts most.
func TestDefaultMemCeilingTracksTheBudgetTheProcessIsUnder(t *testing.T) {
	cases := []struct {
		total uint64
		want  int
	}{
		{0, memCeilingCapMB},           // unreadable ⇒ the built-in, never zero
		{64 << 30, memCeilingCapMB},    // big box: the cap binds at 8 GiB
		{8 << 30, 4096},                // 8 GiB container: half of it
		{2 << 30, memCeilingFloorMB},   // small box: the floor binds
		{512 << 20, memCeilingFloorMB}, // smaller than the floor: still the floor
	}
	for _, tc := range cases {
		if got := defaultMemCeilingMB(tc.total); got != tc.want {
			t.Errorf("total %d ⇒ %d MiB, want %d", tc.total, got, tc.want)
		}
	}
	// The default must never exceed the cap nor fall below the floor, whatever the
	// machine reports.
	for _, total := range []uint64{0, 1, 1 << 20, 1 << 30, 1 << 40, 1 << 50} {
		got := defaultMemCeilingMB(total)
		if got > memCeilingCapMB || got < memCeilingFloorMB {
			t.Errorf("total %d ⇒ %d MiB, outside [%d,%d]", total, got, memCeilingFloorMB, memCeilingCapMB)
		}
	}
}

// TestMemCeilingTripsCleanlyWithAnActionableMessage: the point of the ceiling is that
// kloo stops ITSELF with a diagnosis. Being OOM-killed at 44 GB told the user nothing
// and could take the desktop down. So a trip must name the limit, the observation,
// and the way out — without a rebuild.
func TestMemCeilingTripsCleanlyWithAnActionableMessage(t *testing.T) {
	// The trip is proved by REPORTING a large resident set, not by reaching one.
	// Nothing is allocated: a guard whose test must allocate 8 GiB to prove it works
	// is a guard nobody runs, on a machine that has already had one unplanned restart.
	defer func(saved func() (uint64, error)) { rssProbe = saved }(rssProbe)
	rssProbe = func() (uint64, error) { return 9 << 30, nil } // 9 GiB

	t.Setenv(EnvMemCeilingMB, "") // the computed default, 8 GiB on a large box
	l := &Loop{}
	rss, over := l.observeRSS()
	if !over {
		t.Fatalf("9 GiB resident must trip the default ceiling (%d); rss read as %d", memCeilingBytes(), rss)
	}
	if rss != 9<<30 {
		t.Errorf("rss = %d, want the injected 9 GiB", rss)
	}
	if l.peakRSS == 0 || l.peakRSS < rss {
		t.Errorf("peak RSS not recorded: peak %d, rss %d", l.peakRSS, rss)
	}
	advice := memCeilingAdvice()
	for _, want := range []string{EnvMemCeilingMB, "OOM", "off"} {
		if !strings.Contains(advice, want) {
			t.Errorf("the stop message must mention %q so it is actionable: %q", want, advice)
		}
	}

	// Disabled must be a hard no-op: it can never stop a run.
	t.Setenv(EnvMemCeilingMB, "off")
	if _, over := (&Loop{}).observeRSS(); over {
		t.Error("a disabled ceiling must never report a trip")
	}

	// And the report carries the accounting on an ordinary run too — the peak against
	// the ceiling is what says whether the ceiling is set sensibly.
	t.Setenv(EnvMemCeilingMB, "")
	st := (&Loop{}).memoryStats()
	if st.CeilingBytes <= 0 || st.CeilingSource != "default" || st.RSSBytes == 0 {
		t.Errorf("memoryStats incomplete on a normal run: %+v", st)
	}
}

// TestMemCeilingStopLineIsWellFormed: the report renders evidence as
// "(limit %s, observed %s)", so putting the advice paragraph into Observed stranded
// the limit after a slash at the end of a wall of text. Limit and Observed stay
// short; the advice gets its own line.
func TestMemCeilingStopLineIsWellFormed(t *testing.T) {
	rep := &Report{
		Reason: ReasonBudgetExceeded,
		Steps:  1,
		Budget: &BudgetEvidence{Kind: BudgetMemory, Limit: "8.00 GiB", Observed: "9.00 GiB resident"},
	}
	s := rep.String()
	if !strings.Contains(s, "(limit 8.00 GiB, observed 9.00 GiB resident)") {
		t.Errorf("the limit/observed clause is malformed:\n%s", s)
	}
	// The advice must be present, and on a line of its own rather than inside the
	// parenthetical.
	lines := strings.Split(s, "\n")
	var adviceLine string
	for _, ln := range lines {
		if strings.Contains(ln, EnvMemCeilingMB) {
			adviceLine = ln
		}
	}
	if adviceLine == "" {
		t.Fatalf("the stop does not say how to act on it:\n%s", s)
	}
	if strings.Contains(adviceLine, "observed ") {
		t.Errorf("the advice is still inside the limit/observed clause:\n%s", adviceLine)
	}
	// Another budget kind must be byte-identical to before — the advice is memory-only.
	other := &Report{Reason: ReasonBudgetExceeded, Steps: 1,
		Budget: &BudgetEvidence{Kind: BudgetSteps, Limit: "80", Observed: "81"}}
	if strings.Contains(other.String(), EnvMemCeilingMB) {
		t.Error("a steps-budget stop gained memory advice")
	}
}

// TestRepoMapContentLoadIsAggregateBounded is the real fix. The 2026 OOM fix capped
// the PER-FILE term and left the file-count term unbounded, so the bound on the whole
// load was (file count) x 1 MiB — which is not a bound. Measured: 535 MiB of real
// source loaded here peaks the process at 1.55 GB, linear in the source bytes.
//
// On master loadMapContents does not exist and assembleContext reads every file
// unconditionally, so this fails there by construction.
func TestRepoMapContentLoadIsAggregateBounded(t *testing.T) {
	// Ten nominal 1 MiB files. Nothing is allocated: the budget is decided from the
	// Node sizes the walk already carries, and the reader returns a short string.
	var files []repomap.Node
	for i := 0; i < 10; i++ {
		files = append(files, repomap.Node{Path: fmt.Sprintf("f%02d.go", i), Size: 1 << 20})
	}
	read := func(p string) (string, error) { return "package x\n", nil }

	// A 4 MiB budget must admit exactly four of them and SHED the rest — deterministic,
	// not a crash and not a sample.
	t.Setenv(EnvRepoMapContentMB, "4")
	load := loadMapContents(files, 1<<20, read)
	if len(load.Contents) != 4 {
		t.Errorf("admitted %d files under a 4 MiB budget, want 4", len(load.Contents))
	}
	if load.Skipped != 6 || load.SkippedBytes != 6<<20 {
		t.Errorf("shed accounting = %d files / %d bytes, want 6 / %d", load.Skipped, load.SkippedBytes, 6<<20)
	}
	// Deterministic: the same input must yield the same set, or the repo map changes
	// between turns and re-prefills the whole prompt (~15x on the user's endpoint).
	again := loadMapContents(files, 1<<20, read)
	if len(again.Contents) != len(load.Contents) {
		t.Fatal("the shed is not deterministic")
	}
	for p := range load.Contents {
		if _, ok := again.Contents[p]; !ok {
			t.Errorf("%s admitted on one pass and shed on the next", p)
		}
	}

	// The default must be a NO-OP on every workspace measured: kloo's own tree loads
	// 2.6 MiB of mappable source and the largest real one to hand 535 MiB, both under
	// 512 MiB... except the second, which is why the margin below matters rather than
	// the no-op claim being absolute. It cannot change a benchmark number, since no
	// bench repo is remotely this size.
	t.Setenv(EnvRepoMapContentMB, "")
	if b := repoMapContentBudget(); b != repoMapContentBudgetBytes {
		t.Errorf("default budget = %d, want %d", b, repoMapContentBudgetBytes)
	}
	// THE TWO DEFAULTS MUST BE CONSISTENT WITH EACH OTHER. At the pessimistic end of
	// the measured amplification (7.8x of source bytes), the map default must not
	// imply a peak at or above the process ceiling — otherwise a workspace that
	// legitimately saturates the map budget trips the memory guard as a CONSEQUENCE
	// of the map budget, and the user reads that as a bug in kloo. A 1 GiB map budget
	// failed exactly this check, which is why it is 512 MiB.
	const worstAmplification = 7.8
	t.Setenv(EnvMemCeilingMB, "")
	ceiling := memCeilingBytes()
	if ceiling <= 0 {
		t.Skip("no ceiling in force on this platform")
	}
	budget := int64(repoMapContentBudgetBytes)
	impliedPeak := int64(float64(budget) * worstAmplification)
	if impliedPeak >= ceiling {
		t.Errorf("the map content default (%d B) implies a peak of ~%d B at %.1fx, which is at or above the %d B memory ceiling — a legitimate large workspace would trip the guard because of the map default",
			budget, impliedPeak, worstAmplification, ceiling)
	}
	if margin := float64(ceiling) / float64(impliedPeak); margin < 1.9 {
		t.Errorf("margin between the implied map peak and the memory ceiling is only %.2fx, want >= 2x", margin)
	}
	full := loadMapContents(files, 1<<20, read)
	if len(full.Contents) != len(files) || full.Skipped != 0 {
		t.Errorf("the default budget shed %d of %d files — it must be a no-op at this scale", full.Skipped, len(files))
	}

	// Disabled restores the unbounded pre-v0.26 load, per the sign convention.
	t.Setenv(EnvRepoMapContentMB, "off")
	if b := repoMapContentBudget(); b != 0 {
		t.Errorf("disabled budget = %d, want 0", b)
	}
	if off := loadMapContents(files, 1<<20, read); off.Skipped != 0 {
		t.Errorf("a disabled budget shed %d files", off.Skipped)
	}

	// The per-file cap still applies independently of the aggregate one — keeping the
	// 171fcbf fix honest at the read site.
	t.Setenv(EnvRepoMapContentMB, "")
	huge := []repomap.Node{{Path: "weights.gguf", Size: 46 << 30}, {Path: "a.go", Size: 100}}
	got := loadMapContents(huge, 1<<20, read)
	if _, ok := got.Contents["weights.gguf"]; ok {
		t.Error("a file over the per-file cap was read — this is the 44 GB bug")
	}
	if _, ok := got.Contents["a.go"]; !ok {
		t.Error("a small file beside a huge one must still be mapped")
	}
}

// TestCgroupLimitLowersTheDefaultCeiling: inside a container limited well below host
// RAM, a ceiling computed from MemTotal alone never fires before the cgroup OOM
// killer does — the guard would be present, reported by doctor, and useless.
func TestCgroupLimitLowersTheDefaultCeiling(t *testing.T) {
	host := meminfoTotalBytes()
	if host == 0 {
		t.Skip("no /proc/meminfo on this platform")
	}
	limit := cgroupMemoryLimitBytes()
	total := totalRAMBytes()
	if limit > 0 && limit < host && total != limit {
		t.Errorf("a cgroup limit of %d below host %d must win; totalRAMBytes gave %d", limit, host, total)
	}
	if total > host {
		t.Errorf("totalRAMBytes %d exceeds host MemTotal %d", total, host)
	}
	// The sentinel forms of "no limit" must not be read as a tiny limit.
	dir := t.TempDir()
	for _, body := range []string{"max\n", "9223372036854771712\n", "", "garbage\n"} {
		p := filepath.Join(dir, "memory.max")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		saved := cgroupMemoryPaths
		cgroupMemoryPaths = []string{p}
		got := cgroupMemoryLimitBytes()
		cgroupMemoryPaths = saved
		if got != 0 {
			t.Errorf("%q read as a limit of %d, want 0 (no limit)", body, got)
		}
	}
}
