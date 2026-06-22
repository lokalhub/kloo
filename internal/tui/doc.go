package tui

// Future: kloo as a J1 provider (design-only — NOT wired in v1).
//
// Master plan §1 and design doc §5/§7 list "kloo as a 4th J1 provider" as a
// post-v1 direction: kloo reads a task from stdin, runs autonomously, and emits
// the result to a sink (J1's reportAgentResult). v1 only *designs* the input
// seam so this can be added later by writing a new component — without changing
// the model, Update, or View.
//
// The seam is TaskSource (source.go). Every task entering the program does so as
// a submitTaskMsg, fed by a TaskSource:
//
//   - v1: keyboardSource — the textinput Enter handler emits submitTaskMsg.
//   - future: StdinTaskSource — its Attach() would read tasks from stdin (one
//     per run) and emit submitTaskMsg per line; nothing else in the TUI changes
//     because submitTaskMsg is the single submission channel (commands.go).
//
// The result side (J1's reportAgentResult emission) would hook the terminal
// reportMsg (interrupt.go) — a future StdinTaskSource's run would translate the
// Report into a reportAgentResult to its configured sink. NEITHER the stdin
// reader NOR the reportAgentResult emission exists in v1; the deferral is
// recorded in decisions.md with the design-doc pointers. The source_test.go
// fakeTaskSource demonstrates that a non-keyboard source already drives a task
// submission through the unchanged model — proving the seam admits the future
// StdinTaskSource.
