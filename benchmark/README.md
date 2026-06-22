# kloo acceptance benchmark

This is the **v1 acceptance benchmark** for kloo (overview §6). It is **not** kloo's own
UI — kloo's UI is the Bubble Tea TUI. This is the Ionic app kloo must autonomously build to
prove the whole harness works on a small local model (`snappy`).

## Layout

| Path | Role |
|---|---|
| `fixture/` | The **fixed, untouched** `ionic start tabs --type=angular` skeleton (Tab1/Tab2/Tab3). The deterministic start state. **Do not edit by hand.** |
| `reference/` | A correctly-reworked solution, used only to prove `assert.sh` passes on a right answer. **Not** on kloo's run path (task 02). |
| `assert.sh` | The structural assertion harness — Gate A part 2 (task 02). |
| `artifacts/` | Captured evidence: build tails, versions, assertion runs, live-run transcript, screenshots. |

## The task kloo must do (task 03)

Starting from `fixture/`, kloo with `--model snappy` must autonomously rework the three
tabs into:

| Tab id | Route | Label | Renders |
|---|---|---|---|
| `home` | `/tabs/home` | Home | the word **"Home"**, centered in the viewport |
| `apps` | `/tabs/apps` | Apps | the word **"Apps"**, centered in the viewport |
| `profile` | `/tabs/profile` | Profile | the word **"Profile"**, centered in the viewport |

…with zero `Tab1/Tab2/Tab3` identifiers remaining, distinct icons (`home`/`apps`/`person`),
and each label flex-centered inside `<ion-content>`. The full contract is in
`docs/apps/kloo/conventions/ui-benchmark.md`.

## Two gates (never conflated)

- **Gate A (autonomous):** `npm run build` exits 0 **and** `assert.sh src/` passes. This is
  the loop's success signal.
- **Gate B (human/vision):** real screenshots of each tab read back via vision confirm the
  label is *visually* centered & legible. Never fabricated (`common/conventions/screenshots.md`).

## Toolchain / reproducibility

Exact versions are in `artifacts/versions.txt`. The skeleton was generated with the real
Ionic CLI (7.2.1, via `npx`), Node v22.22.1, Angular 20, `@ionic/angular ^8`. The emitted
skeleton is **NgModule-based** (not standalone): `tabs/tabs.page.html` +
`tabs/tabs-routing.module.ts` + per-tab `tabN/tabN.module.ts`.

```bash
# build the untouched skeleton (Gate A part 1 baseline):
cd fixture && npm install && npm run build   # exits 0, emits www/
```

## Reset to the untouched start state

The fixture is committed; `node_modules/`, `www/`, `dist/`, `.angular/` are gitignored.
To reset after a kloo run mutated it:

```bash
git checkout -- benchmark/fixture        # discard kloo's edits
git clean -fd benchmark/fixture          # drop any new files kloo scaffolded
cd benchmark/fixture && npm install      # restore deps if needed
```

If the workspace is not under git, re-generate from the recorded command in
`artifacts/versions.txt`.
