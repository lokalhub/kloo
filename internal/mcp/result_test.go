package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToResult(t *testing.T) {
	cases := []struct {
		name       string
		res        *sdk.CallToolResult
		wantOutput string
		wantErr    bool
		errSubstr  string
	}{
		{
			name:       "single text",
			res:        &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "hello"}}},
			wantOutput: "hello",
		},
		{
			name: "multiple text blocks joined with newline",
			res: &sdk.CallToolResult{Content: []sdk.Content{
				&sdk.TextContent{Text: "line1"},
				&sdk.TextContent{Text: "line2"},
			}},
			wantOutput: "line1\nline2",
		},
		{
			name: "mixed text + image placeholder",
			res: &sdk.CallToolResult{Content: []sdk.Content{
				&sdk.TextContent{Text: "caption"},
				&sdk.ImageContent{},
			}},
			wantOutput: "caption\n[non-text content: image]",
		},
		{
			name:       "audio placeholder",
			res:        &sdk.CallToolResult{Content: []sdk.Content{&sdk.AudioContent{}}},
			wantOutput: "[non-text content: audio]",
		},
		{
			name:       "resource placeholder",
			res:        &sdk.CallToolResult{Content: []sdk.Content{&sdk.EmbeddedResource{}}},
			wantOutput: "[non-text content: resource]",
		},
		{
			name:      "IsError true carries the text as an error",
			res:       &sdk.CallToolResult{IsError: true, Content: []sdk.Content{&sdk.TextContent{Text: "boom: bad arg"}}},
			wantErr:   true,
			errSubstr: "boom: bad arg",
		},
		{name: "nil result ⇒ empty, no error", res: nil, wantOutput: ""},
		{name: "empty content ⇒ empty, no error", res: &sdk.CallToolResult{}, wantOutput: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := toResult(tc.res)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (output %q)", got.Output)
				}
				if !strings.Contains(err.Error(), tc.errSubstr) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errSubstr)
				}
				if got.Output != "" {
					t.Errorf("error case Output = %q, want empty", got.Output)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Output != tc.wantOutput {
				t.Errorf("Output = %q, want %q", got.Output, tc.wantOutput)
			}
		})
	}
}

func TestCallContextDefault(t *testing.T) {
	ctx, cancel := callContext(context.Background(), 0)
	defer cancel()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected a deadline when timeout ≤ 0 (default applied)")
	}
	remaining := time.Until(dl)
	if remaining <= DefaultCallTimeout-2*time.Second || remaining > DefaultCallTimeout {
		t.Errorf("default deadline remaining = %v, want ≈ %v", remaining, DefaultCallTimeout)
	}
}

func TestCallContextPositive(t *testing.T) {
	ctx, cancel := callContext(context.Background(), 5*time.Second)
	defer cancel()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected a deadline for a positive timeout")
	}
	remaining := time.Until(dl)
	if remaining <= 0 || remaining > 5*time.Second {
		t.Errorf("deadline remaining = %v, want ≈ 5s", remaining)
	}
}

// The package timeout/cap constants exist with their documented values.
func TestPackageConstants(t *testing.T) {
	if DefaultCallTimeout != 30*time.Second {
		t.Errorf("DefaultCallTimeout = %v, want 30s", DefaultCallTimeout)
	}
	if DefaultConnectTimeout != 10*time.Second {
		t.Errorf("DefaultConnectTimeout = %v, want 10s", DefaultConnectTimeout)
	}
	if DefaultMaxExposedTools != 16 {
		t.Errorf("DefaultMaxExposedTools = %d, want 16", DefaultMaxExposedTools)
	}
}
