#!/usr/bin/env bash
# kloo acceptance benchmark — structural assertion harness (Gate A, part 2).
#
# Deterministic, offline, greppable structural gate. Checks a reworked Ionic app's
# src/ tree against the EXACT contract in docs/apps/kloo/conventions/ui-benchmark.md
# §"Required structure". Pure bash + grep — no Node, no Go, no build.
#
#   usage: benchmark/assert.sh <app-src-dir>
#   e.g.:  benchmark/assert.sh benchmark/fixture/src      # untouched skeleton -> FAILs
#          benchmark/assert.sh benchmark/reference/src    # correct solution   -> PASSes
#
# Each of the 5 assertion GROUPS reports PASS/FAIL independently with a reason.
# Exit 0 only if ALL groups pass. This is the artifact kloo's loop (task 03) and
# the reviewers re-run; it asserts STRUCTURE only — visual centering is Gate B.
#
# NOTE: greps target src/ generically (not specific filenames) so the harness is
# robust to either the standalone (tabs.routes.ts) or NgModule (tabs-routing.module.ts)
# starter layout — both are legitimate `ionic start` outputs (see phase discoveries).
set -u

SRC="${1:-}"
if [ -z "$SRC" ] || [ ! -d "$SRC" ]; then
  echo "usage: $0 <app-src-dir>   (got: '${SRC:-<none>}')" >&2
  exit 2
fi

TABS="home apps profile"
# function-mapped icon per tab (common/conventions/ui.md): home->home, apps->apps, profile->person
icon_for() { case "$1" in home) echo home;; apps) echo apps;; profile) echo person;; esac; }
label_for() { case "$1" in home) echo Home;; apps) echo Apps;; profile) echo Profile;; esac; }

fail=0
pass() { printf 'PASS %s\n' "$1"; }
bad()  { printf 'FAIL %s — %s\n' "$1" "$2"; fail=1; }

# template files (html) and style files (scss) under src/
html_files() { find "$SRC" -name '*.html' 2>/dev/null; }

# centering signature: the convention's `.tab-center` class OR an explicit flex-center
# triple (display:flex + align-items:center + justify-content:center) in template/scss.
has_centering() { # $1 = dir/file to search
  if grep -riq 'tab-center' "$1" 2>/dev/null; then return 0; fi
  if grep -riqE 'display:[[:space:]]*flex' "$1" 2>/dev/null \
     && grep -riqE 'align-items:[[:space:]]*center' "$1" 2>/dev/null \
     && grep -riqE 'justify-content:[[:space:]]*center' "$1" 2>/dev/null; then
    return 0
  fi
  return 1
}

# locate the page template for a tab id: an *.html whose path contains the id and
# which is NOT the tabs shell (tabs.page.html). Falls back to any html under app/<id>/.
page_html_for() { # $1 = id
  local id="$1" f
  for f in $(html_files); do
    case "$f" in
      *"$id"*tabs* ) continue;;            # skip the tabs shell variants
      *tabs*"$id"* ) continue;;
      *"$id"* ) echo "$f"; return 0;;
    esac
  done
  return 1
}

echo "== kloo structural assertions =="
echo "src: $SRC"
echo

# ---------------------------------------------------------------------------
# Group 1 — exactly three tabs home/apps/profile with labels Home/Apps/Profile
# ---------------------------------------------------------------------------
btn_count=$(grep -rho 'ion-tab-button' "$SRC" 2>/dev/null | wc -l | tr -d ' ')
# count opening <ion-tab-button ...> tags (each appears as `<ion-tab-button` once)
open_count=$(grep -rhoE '<ion-tab-button[ >]' "$SRC" 2>/dev/null | wc -l | tr -d ' ')
g1=0
if [ "$open_count" -ne 3 ]; then
  bad "1.count" "expected exactly 3 <ion-tab-button>, found $open_count"; g1=1
fi
for id in $TABS; do
  if ! grep -rqE "tab=\"$id\"" "$SRC" 2>/dev/null; then
    bad "1.tab[$id]" "no ion-tab-button/route with tab=\"$id\""; g1=1
  fi
  lbl=$(label_for "$id")
  # label text appears in an ion-label / tab button area
  if ! grep -rqE ">[[:space:]]*$lbl[[:space:]]*<" "$SRC" 2>/dev/null \
     && ! grep -rqiE "<ion-label>[[:space:]]*$lbl" "$SRC" 2>/dev/null; then
    bad "1.label[$lbl]" "tab label '$lbl' not found in the tab bar"; g1=1
  fi
done
# no stray extra tab ids (tab="something-else") beyond the three
strays=$(grep -rhoE 'tab="[^"]+"' "$SRC" 2>/dev/null | sort -u \
         | grep -vE 'tab="(home|apps|profile)"' || true)
if [ -n "$strays" ]; then
  bad "1.exactly3" "unexpected tab ids present: $(echo "$strays" | tr '\n' ' ')"; g1=1
fi
[ "$g1" -eq 0 ] && pass "1.tabs (exactly 3: home/apps/profile, labels Home/Apps/Profile)"

# ---------------------------------------------------------------------------
# Group 2 — each tab page template renders its own name
# ---------------------------------------------------------------------------
g2=0
for id in $TABS; do
  lbl=$(label_for "$id")
  f=$(page_html_for "$id" || true)
  if [ -z "${f:-}" ]; then
    bad "2.page[$id]" "no page template found for tab '$id'"; g2=1; continue
  fi
  if ! grep -qE "(^|[^A-Za-z])$lbl([^A-Za-z]|$)" "$f" 2>/dev/null; then
    bad "2.renders[$id]" "page '$f' does not render the word '$lbl'"; g2=1
  fi
done
[ "$g2" -eq 0 ] && pass "2.renders (each tab page renders its own name)"

# ---------------------------------------------------------------------------
# Group 3 — each tab page has <ion-content> + a flex-centering wrapper
# ---------------------------------------------------------------------------
g3=0
for id in $TABS; do
  f=$(page_html_for "$id" || true)
  [ -z "${f:-}" ] && { bad "3.page[$id]" "no page template for '$id'"; g3=1; continue; }
  dir=$(dirname "$f")
  if ! grep -qE '<ion-content' "$f" 2>/dev/null; then
    bad "3.content[$id]" "page '$f' has no <ion-content>"; g3=1
  fi
  # centering may live in the template (class+inline) or the page's scss
  if ! has_centering "$dir"; then
    bad "3.center[$id]" "no flex-centering wrapper (.tab-center or display:flex+align+justify) in $dir"; g3=1
  fi
done
[ "$g3" -eq 0 ] && pass "3.center (each tab: <ion-content> + flex-centering wrapper)"

# ---------------------------------------------------------------------------
# Group 4 — distinct function-mapped icons home/apps/person (not one repeated)
# ---------------------------------------------------------------------------
g4=0
declare -a seen_icons=()
for id in $TABS; do
  ic=$(icon_for "$id")
  if ! grep -rqE "name=\"$ic\"" "$SRC" 2>/dev/null; then
    bad "4.icon[$id]" "missing function-mapped icon name=\"$ic\""; g4=1
  fi
  seen_icons+=("$ic")
done
uniq_icons=$(printf '%s\n' "${seen_icons[@]}" | sort -u | wc -l | tr -d ' ')
if [ "$uniq_icons" -ne 3 ]; then
  bad "4.distinct" "icons are not 3 distinct function-mapped names"; g4=1
fi
[ "$g4" -eq 0 ] && pass "4.icons (distinct home/apps/person, function-mapped)"

# ---------------------------------------------------------------------------
# Group 5 — zero Tab1/Tab2/Tab3 identifiers anywhere under src/
# ---------------------------------------------------------------------------
hits=$(grep -riE 'tab[123]' "$SRC" 2>/dev/null || true)
if [ -n "$hits" ]; then
  n=$(printf '%s\n' "$hits" | wc -l | tr -d ' ')
  bad "5.no-tab123" "$n line(s) still contain tab1/tab2/tab3 (class/selector/route/file/label)"
  printf '%s\n' "$hits" | sed 's/^/      /' | head -20
else
  pass "5.no-tab123 (grep -ri 'tab[123]' src/ is empty)"
fi

echo
if [ "$fail" -eq 0 ]; then
  echo "RESULT: PASS (all 5 assertion groups green)"
  exit 0
else
  echo "RESULT: FAIL (one or more assertion groups failed)"
  exit 1
fi
