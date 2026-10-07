package agent

import (
	"bufio"
	"errors"
	"os"
	"strconv"
	"strings"
)

// THE PROCESS MEMORY CEILING.
//
// kloo has been OOM-killed once already, at 44 GB: the repo-map walk os.ReadFile'd
// every file it walked with no size cap, and near the user's 46 GB models/
// directory it loaded the lot. That was fixed with a 1 MiB cap in the walk and a
// 5 MiB cap on read_file.
//
// The per-file caps did not close the class, and repomap_budget.go now bounds the
// one that explains that incident: peak RSS during a map assembly runs at a linear
// MULTIPLE of the mappable source bytes (measured at 2.9x on 535 MiB of real source
// here, and at 7.4-7.8x on a synthetic corpus), with only the per-file term capped.
// That is the amplification the guard stands on, and it is the number to cite — not
// a machine-wide cgroup peak, which counts page cache and says nothing about kloo.
//
// Audited at v0.26.0, these still grow without a ceiling (file:line, each verified
// by reading it):
//
//	loop.go:689/917   the `convo` slice is append-only for the whole run and copied
//	                  WHOLE into Report.Transcript at finish, doubling peak RSS at
//	                  the exact moment the run ends
//	mcp/result.go:46  an MCP tool response has no byte cap at all, and is retained
//	                  in convo AND in the TUI transcript
//	builtins.go:176   list_dir materialises every entry of a directory, uncapped
//	loop.go:1373      noOpSigs keys a permanent map on the FULL edit content
//	edit/apply.go:48  the edit_file write path os.ReadFile's uncapped — it does not
//	edit/multi.go:103 go through tools.ReadFile, where the 5 MiB cap lives
//	loop.go:2563      the repo-map content map was per-file capped at 1 MiB and
//	                  TOTAL-uncapped — FIXED in repomap_budget.go, which is the one
//	                  finding from this audit that is bounded rather than merely
//	                  reported, because it is the one that reproduces the 44 GB kill
//
// Chasing each of the rest is the right long-run answer and they are not fixed here.
// What is fixed here is the OUTCOME: whatever grows, kloo notices its own size and
// stops with a diagnosis before the kernel's OOM killer takes it. A kill at 44 GB
// tells the user nothing and can take the desktop with it.
//
// The ceiling is the BACKSTOP, not the fix. An aggregate cap prevents the OOM; a
// ceiling only converts it into a clean abort. Where a bound can be placed, it
// should be, and the ceiling catches what has not been bounded yet.
//
// WHY RSS AND NOT runtime.ReadMemStats. The OOM killer scores processes on resident
// set size. Go's heap statistics are a different quantity in both directions: they
// exclude the runtime's own mappings, stacks, and anything cgo or the allocator has
// not returned to the OS, and they include heap that has been freed but not yet
// released — so HeapAlloc can fall while RSS does not move. A guard meant to fire
// before the kernel does has to measure what the kernel measures. VmRSS in
// /proc/self/status is that number, it is two syscalls and a scan of ~60 short
// lines, and it is read once per step — ~500 reads across a 500-step run, which is
// not worth caching.
//
// It is Linux-only, and that is handled by failing OPEN: on any platform or
// container where /proc/self/status is absent or has no VmRSS line, processRSS
// returns an error and the guard is simply inert. A diagnostic that refuses to run
// the agent because it cannot find a proc file would be a far worse bug than the
// one it is guarding against.

// EnvMemCeilingMB is the field switch. Sign convention, as everywhere else in kloo
// (workingset.go, baseline.go): 0 or unset means the COMPUTED default, a negative
// value DISABLES the guard, and a positive value is that many MiB verbatim. The
// word forms envTri accepts work too, so `KLOO_MEM_CEILING_MB=off` backs the whole
// thing out in the field with no rebuild.
const EnvMemCeilingMB = "KLOO_MEM_CEILING_MB"

// THE GUARD IS ON BY DEFAULT. Stated here because this codebase has a history of
// comments claiming "off by default" for rails that shipped on — loop.go:2862 had
// to be corrected to say KLOO_EDIT_RAIL has been ON since v0.22.0. Unset
// KLOO_MEM_CEILING_MB ⇒ the computed ceiling below is enforced.

// memCeilingCapMB is the upper bound of the computed default: no more than this
// many MiB however large the machine is.
//
// THE DEFAULT IS CHOSEN AGAINST MEASUREMENTS, because a ceiling that trips on a
// legitimate large run is worse than none at all. Measured on this box, running the
// full per-turn map pipeline (repomap.Walk + Extract + the content load + Rank +
// Assemble) in the foreground:
//
//	workspace             files   mappable source   peak RSS
//	kloo's own tree         424           2.6 MiB      24 MB
//	a real monorepo      36,961           535 MiB    1551 MB
//
// So the largest legitimate single operation kloo performs measured at 1.55 GB on a
// workspace two orders of magnitude bigger than its own, and the growth is linear in
// the source bytes, not in the file count. 8 GiB is ~5x that — room for a workspace
// several times larger again — while still being 5.5x BELOW the 44 GB at which kloo
// was actually killed, and an eighth of this machine, so a trip leaves the desktop
// alone. 4 GiB was the first cut and is only 2.6x the measured peak, which is not
// enough margin to tell a bug from a big repo.
const memCeilingCapMB = 8192

// memCeilingShare is the other half of the computed default: never more than this
// fraction of the machine's RAM.
//
// A flat 8 GiB would be window-blind in the way workingset.go's flat constant was
// (see workingSetFor): on an 8 GB CI container or laptop it never trips before the
// kernel does, which is precisely the case where being killed hurts most. kloo
// using half the machine is already pathological, whatever the machine is.
const memCeilingShare = 0.5

// memCeilingFloorMB keeps the computed default from resolving to something a
// legitimate run would cross on a very small box, where half of RAM is less than
// one repo-map assembly. Below this, the honest answer is that the guard cannot
// help and should not be the thing that fails the run.
const memCeilingFloorMB = 1024

// memCeilingBytes is the ceiling in force, in bytes. 0 means disabled (either by
// KLOO_MEM_CEILING_MB being negative, or by this platform having no readable
// /proc/self/status, in which case the value is academic).
func memCeilingBytes() int64 {
	switch n := envQuantityTri(EnvMemCeilingMB, minSensibleCeilingMB); {
	case n < 0:
		return 0 // explicitly disabled
	case n > 0:
		return int64(n) << 20 // an explicit number means that number
	default:
		return int64(defaultMemCeilingMB(totalRAMBytes())) << 20
	}
}

// envQuantityTri is envTri for a knob whose positive values are a QUANTITY rather
// than a flag.
//
// envTri maps the word forms on/true/yes/always to 1, which is right for a
// forced-on boolean and catastrophic here: KLOO_MEM_CEILING_MB=on asked for a 1 MiB
// ceiling, which every live process is already over, so it aborted every run at step
// one. And `=on` is a very plausible thing to type at a guard — more so because the
// guard is already on by default, which makes `=on` a no-op in the user's mind.
//
// So the affirmative words mean "use the built-in default" (0), which is what
// someone typing them wants. The negatives still disable. A bare number is still
// taken verbatim, and anything unreadable still degrades to the default rather than
// to a silent disable.
// minMB is the smallest positive value that could describe a real intent; below it
// the value is treated as the default rather than as an instruction to fail
// everything.
func envQuantityTri(name string, minMB int) int {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "on", "true", "yes", "always", "default":
		return 0
	}
	n := envTri(name)
	if n > 0 && n < minMB {
		// A positive value this small cannot be what anyone wants; it can only break
		// every run. Fall back to the default rather than bricking the tool on a typo —
		// the same fail-toward-working choice envTri makes for unreadable input.
		return 0
	}
	return n
}

// minSensibleCeilingMB is the smallest memory ceiling that could describe a real
// intent. kloo's baseline RSS is ~10 MiB and one repo-map assembly on its own tree is
// ~24 MiB, so anything below this stops step one on every workspace.
const minSensibleCeilingMB = 64

// defaultMemCeilingMB computes the default from the machine's RAM. totalBytes of 0
// (unreadable /proc/meminfo) falls back to the flat cap, which is the pre-existing
// behaviour of every other kloo default: degrade to the built-in, never to zero.
func defaultMemCeilingMB(totalBytes uint64) int {
	if totalBytes == 0 {
		return memCeilingCapMB
	}
	mb := int(float64(totalBytes>>20) * memCeilingShare)
	if mb > memCeilingCapMB {
		mb = memCeilingCapMB
	}
	if mb < memCeilingFloorMB {
		mb = memCeilingFloorMB
	}
	return mb
}

// MemCeilingBytes, MemCeilingSource and ProcessRSSBytes are the read-only accessors
// `kloo doctor` reports through. Exported here rather than recomputed in the CLI for
// the same reason the context gauge measures the assembled prompt: a diagnostic that
// derives its answer independently of the thing it describes eventually describes
// something else. doctor must print the number the run will enforce.
func MemCeilingBytes() int64 { return memCeilingBytes() }

func MemCeilingSource() string {
	switch {
	case memCeilingBytes() == 0:
		return "disabled"
	case envQuantityTri(EnvMemCeilingMB, minSensibleCeilingMB) > 0:
		return "env"
	default:
		return "default"
	}
}

// ProcessRSSBytes is the resident set now, or 0 where it cannot be read (which is
// also what makes the guard inert there).
func ProcessRSSBytes() uint64 {
	rss, _ := processRSS()
	return rss
}

// RepoMapContentBudgetBytes is the aggregate cap on the repo-map content load in
// force; 0 means disabled.
func RepoMapContentBudgetBytes() int64 { return repoMapContentBudget() }

// MemoryGuardStats is the process-memory accounting for the report and the run JSON.
type MemoryGuardStats struct {
	// CeilingBytes is the ceiling in force; 0 means the guard is off (disabled by
	// KLOO_MEM_CEILING_MB, or no readable VmRSS on this platform).
	CeilingBytes int64 `json:"ceiling_bytes"`
	// CeilingSource is how the ceiling was chosen: "default" (computed from the
	// lesser of MemTotal and any cgroup limit), "env" (KLOO_MEM_CEILING_MB), or
	// "disabled". Named because a reported number whose provenance is invisible is
	// how `kloo doctor` came to print a working-set cap that bound nothing.
	CeilingSource string `json:"ceiling_source"`
	PeakRSSBytes  uint64 `json:"peak_rss_bytes"`
	RSSBytes      uint64 `json:"rss_bytes"`
	// MapContentBytes is the workspace source held for the last repo-map assembly and
	// MapContentSkippedFiles how many files its aggregate budget shed
	// (repomap_budget.go). The second being nonzero means the map describes less of
	// the repo than it could, which should never be inferred from a crash.
	MapContentBytes        int64 `json:"map_content_bytes"`
	MapContentSkippedFiles int   `json:"map_content_skipped_files"`
}

// memoryStats collects the run's memory accounting for the report.
func (l *Loop) memoryStats() MemoryGuardStats {
	rss, _ := processRSS()
	if rss > l.peakRSS {
		l.peakRSS = rss
	}
	ceiling := memCeilingBytes()
	source := "default"
	switch {
	case ceiling == 0:
		source = "disabled"
	case envQuantityTri(EnvMemCeilingMB, minSensibleCeilingMB) > 0:
		source = "env"
	}
	return MemoryGuardStats{
		CeilingBytes:           ceiling,
		CeilingSource:          source,
		PeakRSSBytes:           l.peakRSS,
		RSSBytes:               rss,
		MapContentBytes:        l.mapContentBytes,
		MapContentSkippedFiles: l.mapContentSkipped,
	}
}

// ErrNoRSS reports that this platform has no readable VmRSS, which makes the guard
// inert rather than failing the run.
var ErrNoRSS = errors.New("agent: no VmRSS available on this platform")

// processRSS is the process's resident set size in bytes, from /proc/self/status.
//
// VmRSS, not VmHWM: the ceiling is about what kloo is holding NOW, so a run that
// peaked during one repo-map assembly and gave the memory back should not be
// stopped for it on step 300.
func processRSS() (uint64, error) { return rssProbe() }

// rssProbe is the reading in force. A var so a test can exercise the CEILING
// without allocating: the trip is proved by reporting a large RSS, not by reaching
// one. The machine this was written on had already had one unplanned restart, and a
// guard whose test must allocate 8 GiB to prove it works is a guard nobody runs.
// Never set outside tests.
var rssProbe = func() (uint64, error) { return rssFrom("/proc/self/status") }

// rssFrom is processRSS with the path injected, so the parser is tested against
// real /proc text fixtures without needing a process of a given size — and so the
// ceiling can be exercised without allocating anything. Deliberate: the machine
// this was written on had already had one restart that day, and a guard whose test
// must allocate 8 GiB to prove it works is a guard nobody will run.
func rssFrom(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, ErrNoRSS
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		// "VmRSS:    18180 kB" — the unit is always kB in the kernel's own
		// formatter, but parse it rather than assume, so a future unit does not
		// silently become a 1024x under-report (which would read as "plenty of room").
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, ErrNoRSS
		}
		n, perr := strconv.ParseUint(fields[1], 10, 64)
		if perr != nil {
			return 0, ErrNoRSS
		}
		mult := uint64(1024) // kB
		if len(fields) >= 3 {
			switch strings.ToLower(fields[2]) {
			case "kb":
				mult = 1024
			case "mb":
				mult = 1024 * 1024
			case "b":
				mult = 1
			}
		}
		return n * mult, nil
	}
	return 0, ErrNoRSS
}

// totalRAMBytes is the memory kloo is actually allowed to use: the LOWER of the
// machine's MemTotal and any cgroup limit applied to this process.
//
// The cgroup half matters because the share-of-RAM default is otherwise wrong in
// exactly the environment where being OOM-killed is most likely. In a container
// limited to 2 GiB on a 64 GiB host, MemTotal alone computes a ceiling of 8 GiB and
// the guard never fires before the cgroup OOM killer does — the guard would be
// present, reported by doctor, and useless. Taking the minimum means the default
// always describes the budget the process is really under.
func totalRAMBytes() uint64 {
	host := meminfoTotalBytes()
	limit := cgroupMemoryLimitBytes()
	switch {
	case host == 0:
		return limit
	case limit == 0 || limit > host:
		return host
	default:
		return limit
	}
}

// meminfoTotalBytes is MemTotal from /proc/meminfo, or 0 when it cannot be read.
func meminfoTotalBytes() uint64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		n, perr := strconv.ParseUint(fields[1], 10, 64)
		if perr != nil {
			return 0
		}
		return n * 1024 // kB
	}
	return 0
}

// cgroupMemoryPaths are the files a container memory limit can live in: cgroup v2
// first (memory.max, the modern unified hierarchy), then the v1 location. 0 when
// neither exists or the limit is "max" / absurdly large, which is how both
// hierarchies spell "no limit" (v1 uses a sentinel near 2^63).
//
// KNOWN LIMITATION, stated rather than implied: only the ROOT of the mounted
// hierarchy is read. A limit applied to a NESTED cgroup — `systemd-run
// -p MemoryMax=`, or a k8s pod whose cgroupfs is not namespaced to the container —
// is invisible here, because finding it means resolving this process's own path from
// /proc/self/cgroup and walking up. The common container case (a namespaced
// hierarchy, where the container's limit IS the root) is covered. Where it is not,
// the default simply falls back to the host share, which is the pre-existing
// behaviour and never tighter than it should be — so the failure mode is a ceiling
// that is too loose, not one that stops a legitimate run.
var cgroupMemoryPaths = []string{
	"/sys/fs/cgroup/memory.max",
	"/sys/fs/cgroup/memory/memory.limit_in_bytes",
}

func cgroupMemoryLimitBytes() uint64 {
	for _, p := range cgroupMemoryPaths {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(raw))
		if s == "" || s == "max" {
			continue
		}
		n, perr := strconv.ParseUint(s, 10, 64)
		if perr != nil || n == 0 {
			continue
		}
		// v1's "unlimited" is PAGE_COUNTER_MAX scaled to bytes — a number in the
		// exabytes. Anything at or above a petabyte is that sentinel, not a limit.
		if n >= 1<<50 {
			continue
		}
		return n
	}
	return 0
}

// observeRSS samples the process's resident set at a step boundary and reports
// whether the ceiling is now exceeded. It also records the run's peak, which goes
// into the report whether or not the guard fires: "how close did this run get" is
// what says whether the ceiling is set sensibly.
//
// A ceiling of 0 (disabled) or an unreadable VmRSS makes it a pure no-op that
// cannot stop a run — the guard never fails a run because it could not measure.
func (l *Loop) observeRSS() (uint64, bool) {
	rss, err := processRSS()
	if err != nil || rss == 0 {
		return 0, false
	}
	if rss > l.peakRSS {
		l.peakRSS = rss
	}
	ceiling := memCeilingBytes()
	return rss, ceiling > 0 && rss > uint64(ceiling)
}

// humanBytes renders a byte count in MiB/GiB, because a stop message that says
// "8589934592" makes the reader do arithmetic to find out whether the number is
// alarming.
func humanBytes(n uint64) string {
	switch {
	case n >= 1<<30:
		return strconv.FormatFloat(float64(n)/float64(1<<30), 'f', 2, 64) + " GiB"
	case n >= 1<<20:
		return strconv.FormatUint(n>>20, 10) + " MiB"
	default:
		return strconv.FormatUint(n, 10) + " B"
	}
}

// memCeilingAdvice is the actionable half of the stop message. The ceiling trips
// because something retained memory, and the user's two real choices are to find
// out what or to raise the bar — so the message names the switch rather than
// leaving them to grep for it.
func memCeilingAdvice() string {
	return "kloo stopped itself before the kernel's OOM killer could: resident memory crossed the ceiling. " +
		"This is usually a huge tool output, an MCP response, or a repo map over a very large workspace being retained for the whole run. " +
		"Narrow the workspace (--scope, or run from a subdirectory), or raise the ceiling with " +
		EnvMemCeilingMB + "=<MiB>; " + EnvMemCeilingMB + "=off disables the guard entirely."
}
