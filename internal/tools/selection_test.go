package tools

import (
	"errors"
	"testing"
)

func TestSelectAdapter(t *testing.T) {
	cases := []struct {
		name       string
		toolFormat string
		caps       EndpointCaps
		want       string // "native" | "xml"
		wantErr    bool
	}{
		{name: "native default + tools-capable → native", toolFormat: "native", caps: EndpointCaps{SupportsTools: true}, want: "native"},
		{name: "auto + tools-capable → native", toolFormat: "", caps: EndpointCaps{SupportsTools: true}, want: "native"},
		{name: "auto + no tool support → xml", toolFormat: "", caps: EndpointCaps{SupportsTools: false}, want: "xml"},
		// "auto" is the explicit synonym for "" — regression rows for the crash a
		// real user hit (toolFormat:"auto" used to error). Must behave like "".
		{name: "literal auto + tools-capable → native", toolFormat: "auto", caps: EndpointCaps{SupportsTools: true}, want: "native"},
		{name: "literal auto + no tool support → xml", toolFormat: "auto", caps: EndpointCaps{SupportsTools: false}, want: "xml"},
		{name: "xml override forces xml even when tools-capable", toolFormat: "xml", caps: EndpointCaps{SupportsTools: true}, want: "xml"},
		{name: "native override forces native even without tool support", toolFormat: "native", caps: EndpointCaps{SupportsTools: false}, want: "native"},
		{name: "trained maps to native", toolFormat: "trained", caps: EndpointCaps{SupportsTools: true}, want: "native"},
		{name: "yaml is rejected", toolFormat: "yaml", caps: EndpointCaps{SupportsTools: true}, wantErr: true},
		{name: "unknown is rejected", toolFormat: "bananas", caps: EndpointCaps{}, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ad, err := SelectAdapter(tc.toolFormat, tc.caps)
			if tc.wantErr {
				if !errors.Is(err, ErrUnsupportedToolFormat) {
					t.Fatalf("want ErrUnsupportedToolFormat, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			gotKind := "xml"
			if _, ok := ad.(NativeFCAdapter); ok {
				gotKind = "native"
			}
			if gotKind != tc.want {
				t.Errorf("adapter = %s, want %s", gotKind, tc.want)
			}
		})
	}
}
