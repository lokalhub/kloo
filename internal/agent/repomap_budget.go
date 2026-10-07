package agent

import (
	"github.com/lokalhub/kloo/internal/repomap"
)

// THE AGGREGATE CAP ON THE REPO-MAP CONTENT LOAD.
//
// assembleContext reads every mapped file's bytes into one map[string][]byte and
// hands the whole thing to repomap.Rank, which needs the text to build the def→ref
// graph for the PageRank centrality signal. The 2026 OOM fix (171fcbf) capped the
// PER-FILE term at 1 MiB — walk.go:47 excludes anything larger and loop.go:2529
// re-checks it at the read site — and left the FILE-COUNT term unbounded. So the
// bound on the whole load was:
//
//	total ≤ (number of mappable files) × 1 MiB
//
// which is not a bound. Measured in the foreground on this box:
//
//	workspace          mappable source   peak RSS   amplification
//	kloo's own tree            2.6 MiB      24 MB   —
//	a real monorepo            535 MiB    1551 MB   2.9x of source bytes
//
// and independently on a synthetic corpus at 63 MiB and 126 MiB, 7.8x and 7.4x —
// linear in the SOURCE BYTES either way, with the constant depending on how many
// symbols and references the text yields. Peak RSS is some multiple of the mappable
// source and nothing stops it. Extrapolated, a workspace with a few GiB of in-jail
// source takes this machine out, which explains the 44 GB kill near a 46 GB models/
// directory far better than any leak does: the per-file cap stopped one .gguf being
// read whole, and never bounded the sum.
//
// SHEDDING IS DETERMINISTIC, NOT A CRASH AND NOT A SAMPLE. Ranking already has to
// choose what goes in the map, so running out of content budget is the same kind of
// decision: the files are visited in repomap.Walk's order, which is lexicographic by
// path (walk.go:70 sorts), so the same workspace always yields the same contents map
// and the repo map stays byte-stable turn to turn. Byte-stability is not a nicety
// here — a map that changes between turns re-prefills the whole prompt, which on the
// user's lokalai is 1.05s against 17.37s (~15x), and freezing the map is what took
// two bench cases from 8/16 to 16/16.
//
// A file that does not fit the budget is simply omitted from `contents`, which is
// the behaviour that already existed for an unreadable or over-cap file: it
// contributes no graph references and is still ranked by its path and symbols, and
// is still reachable by read_file/list_dir. Degrading the SIGNAL is the documented
// contract of this pipeline ("degrade to empty context, the loop still runs"); dying
// is not.

// repoMapContentBudgetBytes bounds the TOTAL bytes of workspace source held in
// memory for one map assembly.
//
// 512 MiB is chosen to be a NO-OP on every workspace measured — kloo's own tree
// loads 2.6 MiB and the largest real workspace to hand loads 535 MiB of mappable
// source — while keeping a margin against the PROCESS ceiling next door.
//
// THE TWO DEFAULTS HAVE TO BE CONSISTENT WITH EACH OTHER, which is why this is not
// 1 GiB. At the pessimistic end of the measured amplification (7.4-7.8x of source
// bytes), 1 GiB of content is ~7.5-8 GB of peak RSS — i.e. right at the 8 GiB
// memory ceiling. A workspace that legitimately saturated the map default would then
// trip the memory guard as a CONSEQUENCE of the map default, and the user would
// correctly read that as a bug in kloo. 512 MiB restores a 2x margin at the
// pessimistic constant (~4 GB against an 8 GiB ceiling) and ~1.5 GB at the constant
// measured on real source.
//
// Sign convention as everywhere else in kloo (workingset.go, memguard.go): 0 means
// the built-in default and a negative value disables the cap, restoring the
// pre-v0.26 unbounded load.
const repoMapContentBudgetBytes = 512 << 20 // 512 MiB

// minSensibleMapContentMB is the floor on an explicit setting, for the same reason
// the memory ceiling has one: KLOO_MAP_CONTENT_MB=on parsed through envTri would mean
// 1 MiB and silently reduce the repo map to a handful of files.
const minSensibleMapContentMB = 1

// EnvRepoMapContentMB backs the cap out in the field with no rebuild, in MiB.
// 0/unset ⇒ the default above, negative ⇒ disabled (unbounded, the old behaviour).
const EnvRepoMapContentMB = "KLOO_MAP_CONTENT_MB"

// repoMapContentBudget resolves the cap in bytes. 0 ⇒ no cap.
func repoMapContentBudget() int64 {
	switch n := envQuantityTri(EnvRepoMapContentMB, minSensibleMapContentMB); {
	case n < 0:
		return 0
	case n > 0:
		return int64(n) << 20
	default:
		return repoMapContentBudgetBytes
	}
}

// contentLoad is the result of reading the mapped files under the aggregate budget.
// Skipped and SkippedBytes are reported rather than inferred: a map that quietly
// stopped describing most of the repo should be visible, which is the lesson of the
// working-set cap that reported itself "BINDING" while binding nothing.
type contentLoad struct {
	Contents map[string][]byte
	// Bytes is what was actually read and is being held. Reserved is what the budget
	// was charged, which is the sum of the Node SIZES — the two differ only where a
	// file changed between the walk and the read, and the budget has to be charged
	// the stat because the decision is made BEFORE the read. Charging it the bytes
	// read instead is a bug that disables the cap entirely: the allocation has
	// already happened by the time you can measure it.
	Bytes        int64
	Reserved     int64
	Skipped      int
	SkippedBytes int64
	Budget       int64
}

// loadMapContents reads each mapped file through the caller's reader (the jailed
// workspace — never a raw os.ReadFile, which would break the path jail) until the
// aggregate budget is spent.
//
// files must be in repomap.Walk order so the shed is deterministic. read returns the
// file's content or an error; an error is skipped exactly as before.
//
// perFileCap is applied here as well as at walk time, keeping the 171fcbf fix honest
// at the read site (the comment on loop.go's repoMapFileCap).
func loadMapContents(files []repomap.Node, perFileCap int64, read func(path string) (string, error)) contentLoad {
	out := contentLoad{Contents: map[string][]byte{}, Budget: repoMapContentBudget()}
	for _, f := range files {
		if f.Size > perFileCap {
			continue // unchanged: over the per-file cap, never read
		}
		// Check the budget against the file's KNOWN SIZE before reading it, not after.
		// Reading first and then discarding would allocate the very bytes the cap
		// exists to prevent — the cap has to be decided from the stat, which Walk
		// already carries on the Node (walk.go:51: "so later tasks can budget without
		// re-statting").
		if out.Budget > 0 && out.Reserved+f.Size > out.Budget {
			out.Skipped++
			out.SkippedBytes += f.Size
			continue
		}
		data, err := read(f.Path)
		if err != nil {
			continue // unchanged: unreadable file contributes no graph references
		}
		out.Contents[f.Path] = []byte(data)
		out.Bytes += int64(len(data))
		out.Reserved += f.Size
	}
	return out
}
