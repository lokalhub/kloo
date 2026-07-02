package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/lokalhub/kloo/internal/config"
)

// TestDoctorReportsPatchOnlyAndStopOn: `kloo doctor` surfaces patch_only (A4 DoD)
// and the resolved stop_on rules in both JSON and human output.
func TestDoctorReportsPatchOnlyAndStopOn(t *testing.T) {
	cfg := config.Config{
		Model:      "m",
		Endpoint:   "http://x/v1",
		PatchOnly:  true,
		ScopeAllow: []string{"src/**"},
		StopOn:     config.StopPolicy{OffScopeEdit: true, RepeatedVerify: 2},
	}
	diag := buildResolvedConfigDiagnostic(cfg, "", "", lintOpts{Disabled: true})
	if !diag.PatchOnly {
		t.Fatal("doctor must report patch_only=true")
	}
	if !diag.StopOn.OffScopeEdit || diag.StopOn.RepeatedVerify != 2 {
		t.Fatalf("doctor stop_on = %+v", diag.StopOn)
	}

	var jbuf bytes.Buffer
	if err := writeDoctorJSON(&jbuf, diag); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(jbuf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["patch_only"] != true {
		t.Fatalf("json patch_only = %v", m["patch_only"])
	}

	var hbuf bytes.Buffer
	writeDoctorHuman(&hbuf, diag)
	if !bytes.Contains(hbuf.Bytes(), []byte("patch_only: true")) {
		t.Fatalf("human output missing patch_only line:\n%s", hbuf.String())
	}
	if !bytes.Contains(hbuf.Bytes(), []byte("stop_on:")) {
		t.Fatalf("human output missing stop_on line:\n%s", hbuf.String())
	}
}
