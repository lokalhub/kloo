package edit

import (
	"fmt"
	"os"
)

// THE EDIT PATH'S OWN READ CAP.
//
// The 5 MiB cap that bounds reading a workspace file lives in tools.ReadFile
// (tools/files.go:40). The EDIT path does not go through it: ApplyToFile
// (apply.go) and applyMulti (multi.go) call os.ReadFile directly, so edit_file and
// write_file on a huge file were entirely uncapped. That is the same shape as the
// bug that OOM-killed kloo at 44 GB — a read with no stat check — one package over
// from where it was fixed.
//
// WHY THE PROCESS CEILING DOES NOT COVER THIS, which is the whole reason it is
// capped here rather than deferred with the other audit findings. The RSS ceiling
// (agent/memguard.go) is sampled at the STEP BOUNDARY. An allocation that begins and
// completes inside a single tool call is structurally invisible to it: the reading is
// taken before the call and after it, and the spike in between is never seen. And
// this is a spike, not retention — ApplyBlock builds the output string beside the
// input, so peak is ~3x the file size (file + staged copy + result) within one call.
// A 500 MB checked-in lockfile or test fixture is ~1.5 GB the ceiling will never
// observe.
//
// So the audit's rows split into two classes, and only one of them is covered by a
// step-boundary ceiling:
//
//	RETENTION (ceiling catches it, deferred deliberately): the append-only convo
//	  slice and its whole-copy into Report.Transcript, the TUI transcript, bgProc
//	  entries that are never reaped, noOpSigs keyed on full edit content. These grow
//	  across steps, so a step-boundary sample sees them.
//	SPIKE (ceiling CANNOT catch it): this, and an MCP response
//	  (mcp/result.go:46) — which is left uncapped for now because bounding it changes
//	  model-visible content and deserves its own measured change. The ceiling does NOT
//	  cover that row, and this comment says so rather than implying otherwise.
//
// maxEditFileBytes mirrors tools/files.go:40 deliberately rather than importing it:
// internal/edit is below internal/tools in the dependency order and must not depend
// on it. The two are pinned together by TestEditReadCapMatchesReadFileCap.
const maxEditFileBytes = 5 << 20 // 5 MiB

// readFileCapped is os.ReadFile with the stat check the edit path never had.
//
// The error names the size, the cap and a way forward, because this refusal lands in
// front of a model that will otherwise retry the identical call: the actionable
// advice is to edit a smaller region, not to try again.
func readFileCapped(path string) ([]byte, error) {
	if info, err := os.Stat(path); err == nil && info.Size() > maxEditFileBytes {
		return nil, fmt.Errorf("edit: %s is %d bytes (cap %d) — too large to edit whole; "+
			"use run_command with sed/awk to change it in place, or narrow the edit to a smaller file",
			path, info.Size(), maxEditFileBytes)
	}
	return os.ReadFile(path)
}
