package repomap

import (
	"strings"
	"testing"
)

// rankedFixtures builds a deterministic ranked list with descending scores and
// known block sizes for budget tests.
func sampleRanked() []RankedFile {
	return []RankedFile{
		{Path: "a/top.ts", Score: 30, Symbols: []Symbol{{Name: "Top", Kind: KindClass, File: "a/top.ts", Line: 1}}},
		{Path: "b/mid.ts", Score: 20, Symbols: []Symbol{{Name: "Mid", Kind: KindClass, File: "b/mid.ts", Line: 1}}},
		{Path: "c/low.ts", Score: 10, Symbols: []Symbol{{Name: "Low", Kind: KindClass, File: "c/low.ts", Line: 1}}},
	}
}

func TestAssembleNeverExceedsBudget(t *testing.T) {
	ranked := sampleRanked()
	for _, budget := range []int{0, 1, 5, 10, 20, 100, 1000} {
		ctx, stat := Assemble(ranked, budget, ApproxTokens)
		if ApproxTokens(ctx) > budget {
			t.Errorf("budget %d: assembled context is %d tokens (over budget)", budget, ApproxTokens(ctx))
		}
		if stat.TokensUsed > budget {
			t.Errorf("budget %d: reported TokensUsed %d > budget", budget, stat.TokensUsed)
		}
		if stat.TokensUsed != ApproxTokens(ctx) {
			t.Errorf("budget %d: stat.TokensUsed %d != actual %d", budget, stat.TokensUsed, ApproxTokens(ctx))
		}
	}
}

func TestAssembleEvictsLowestRankFirst(t *testing.T) {
	ranked := sampleRanked()
	// A budget that fits exactly the top two whole blocks.
	twoBlocks := ApproxTokens(RenderFileBlock(ranked[0])) + ApproxTokens(RenderFileBlock(ranked[1]))

	ctx, stat := Assemble(ranked, twoBlocks, ApproxTokens)
	if !strings.Contains(ctx, "a/top.ts") || !strings.Contains(ctx, "b/mid.ts") {
		t.Errorf("top-2 not retained: %q", ctx)
	}
	if strings.Contains(ctx, "c/low.ts") {
		t.Errorf("lowest-rank file should be evicted first: %q", ctx)
	}
	if len(stat.Dropped) != 1 || stat.Dropped[0] != "c/low.ts" {
		t.Errorf("expected c/low.ts dropped, got %v", stat.Dropped)
	}
}

func TestAssembleLargerBudgetIncludesMore(t *testing.T) {
	ranked := sampleRanked()
	smallBudget := ApproxTokens(RenderFileBlock(ranked[0]))
	_, small := Assemble(ranked, smallBudget, ApproxTokens)
	_, large := Assemble(ranked, 1000, ApproxTokens)
	if len(large.Included) <= len(small.Included) {
		t.Errorf("larger budget should include more: small=%d large=%d", len(small.Included), len(large.Included))
	}
	if len(large.Included) != 3 {
		t.Errorf("large budget should include all 3, got %d", len(large.Included))
	}
}

func TestAssembleWindowsOverLargeHighRankFile(t *testing.T) {
	// A single high-rank file far larger than the budget must be WINDOWED, not
	// dropped, and must not bust the budget.
	big := strings.Repeat("x", 4000)
	ranked := []RankedFile{
		{Path: "huge.ts", Score: 99, Symbols: []Symbol{{Name: big, Kind: KindClass, File: "huge.ts", Line: 1}}},
	}
	budget := 50
	ctx, stat := Assemble(ranked, budget, ApproxTokens)

	if ApproxTokens(ctx) > budget {
		t.Errorf("windowed file busted budget: %d > %d", ApproxTokens(ctx), budget)
	}
	if !stat.Truncated {
		t.Errorf("expected Truncated=true for the windowed high-rank file")
	}
	if len(stat.Included) != 1 || stat.Included[0] != "huge.ts" {
		t.Errorf("over-large high-rank file should be included (windowed), got included=%v", stat.Included)
	}
	if !strings.Contains(ctx, "truncated") {
		t.Errorf("windowed content should carry a truncation marker: %q", ctx)
	}
}

func TestAssembleReportedStatObservable(t *testing.T) {
	ranked := sampleRanked()
	ctx, stat := Assemble(ranked, 1000, ApproxTokens)
	if stat.Budget != 1000 || stat.TokensUsed != ApproxTokens(ctx) {
		t.Errorf("stat not observable/consistent: %+v vs actual %d", stat, ApproxTokens(ctx))
	}
	if s := stat.FormatStat(); !strings.Contains(s, "tokens") {
		t.Errorf("FormatStat unexpected: %q", s)
	}
}

func TestAssembleZeroBudgetDropsAll(t *testing.T) {
	ranked := sampleRanked()
	ctx, stat := Assemble(ranked, 0, nil)
	if ctx != "" || len(stat.Included) != 0 || len(stat.Dropped) != 3 {
		t.Errorf("zero budget should drop all: ctx=%q stat=%+v", ctx, stat)
	}
}
