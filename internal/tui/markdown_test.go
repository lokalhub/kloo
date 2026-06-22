package tui

import "testing"

// TestAssistantMarkdownStyling (task 04): headers, bold, inline code, and bullets
// render with markers stripped/replaced under the ascii profile.
func TestAssistantMarkdownStyling(t *testing.T) {
	content := "# Plan\nI'll rename the **three** tabs. Run `npm run build`.\n- edit tab1\n* edit tab2"
	v := apply(tall(), streamDeltaMsg{Content: content}, streamDoneMsg{}).View()

	present := []string{"Plan", "three", "npm run build", "• edit tab1", "• edit tab2"}
	for _, s := range present {
		if !contains(v, s) {
			t.Errorf("markdown frame missing %q:\n%s", s, v)
		}
	}
	stripped := []string{"# Plan", "**three**", "`npm run build`", "- edit tab1", "* edit tab2"}
	for _, s := range stripped {
		if contains(v, s) {
			t.Errorf("markdown marker %q should be stripped/replaced:\n%s", s, v)
		}
	}
	requireGolden(t, "assistant-markdown.golden", v)
}

// TestAssistantPlainUnchanged (task 04): prose with no markers passes through the
// styler unchanged.
func TestAssistantPlainUnchanged(t *testing.T) {
	content := "Just plain prose with no markers at all.\nSecond line here."
	v := apply(tall(), streamDeltaMsg{Content: content}, streamDoneMsg{}).View()
	for _, want := range []string{"Just plain prose with no markers at all.", "Second line here."} {
		if !contains(v, want) {
			t.Errorf("plain prose altered, missing %q:\n%s", want, v)
		}
	}
	requireGolden(t, "assistant-plain.golden", v)
}

// TestAssistantPartialMarkerLiteral (task 04): a mid-stream unterminated marker
// degrades to literal text — no panic, no dangling style.
func TestAssistantPartialMarkerLiteral(t *testing.T) {
	// No streamDoneMsg: the item is still streaming with an unclosed **.
	v := apply(tall(), streamDeltaMsg{Content: "rename the **three"}).View()
	if !contains(v, "**three") {
		t.Errorf("unterminated bold should render literally:\n%s", v)
	}
	if !contains(v, "(streaming…)") {
		t.Errorf("partial assistant item should still show the streaming marker:\n%s", v)
	}
}

// TestStylizeMarkdownUnit exercises the styler directly for edge cases.
func TestStylizeMarkdownUnit(t *testing.T) {
	cases := []struct{ in, want string }{
		{"# Header", "Header"},                         // marker stripped (ascii: plain)
		{"## Sub", "Sub"},                              // ## stripped
		{"- item", "• item"},                           // bullet replaced
		{"* item", "• item"},                           // alt bullet replaced
		{"plain line", "plain line"},                   // unchanged
		{"a **b** c", "a b c"},                         // bold markers stripped
		{"use `x` now", "use x now"},                   // code backticks stripped
		{"open **unterminated", "open **unterminated"}, // literal
		{"#nospace", "#nospace"},                       // not a header (no space)
	}
	for _, tc := range cases {
		if got := stylizeMarkdown(tc.in); got != tc.want {
			t.Errorf("stylizeMarkdown(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
