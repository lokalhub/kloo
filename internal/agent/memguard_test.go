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
		{"1", 1 << 20},
		{"nonsense", -1}, // unreadable ⇒ the default, never a silent disable
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
	// 1 MiB ceiling: any live process is over it, so the trip is observed rather than
	// simulated, and nothing is allocated to get there.
	t.Setenv(EnvMemCeilingMB, "1")
	l := &Loop{}
	rss, over := l.observeRSS()
	if !over {
		t.Fatalf("a 1 MiB ceiling must trip; rss was %d", rss)
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
	// 2.6 MiB and the largest real one to hand 535 MiB, both far under 1 GiB. So this
	// cannot change any benchmark number.
	t.Setenv(EnvRepoMapContentMB, "")
	if b := repoMapContentBudget(); b != repoMapContentBudgetBytes {
		t.Errorf("default budget = %d, want %d", b, repoMapContentBudgetBytes)
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
