package config

import (
	"testing"
	"time"
)

// TestResolveVerifyTimeoutPrecedence pins the verify gate's own ceiling on the same
// flag → env → profile → default ladder every other timeout rides. It exists because
// the ceiling was previously a compile-time constant: a gate that needed twelve
// minutes (a cold Rust build of seven test binaries) was killed at five, and the run
// threw away a correct nine-file fix with no knob anywhere to raise it.
func TestResolveVerifyTimeoutPrecedence(t *testing.T) {
	profile := writeProfile(t, `{"local":{"verifyTimeout":"8m"}}`)

	t.Run("default when nothing sets it", func(t *testing.T) {
		got, err := Resolve(Flags{}, envFunc(nil), writeProfile(t, `{}`))
		if err != nil {
			t.Fatal(err)
		}
		if got.VerifyTimeout != DefaultVerifyTimeout {
			t.Errorf("VerifyTimeout = %s, want the default %s", got.VerifyTimeout, DefaultVerifyTimeout)
		}
	})

	t.Run("profile beats the default", func(t *testing.T) {
		got, err := Resolve(Flags{}, envFunc(nil), profile)
		if err != nil {
			t.Fatal(err)
		}
		if got.VerifyTimeout != 8*time.Minute {
			t.Errorf("VerifyTimeout = %s, want 8m from the profile", got.VerifyTimeout)
		}
	})

	t.Run("env beats the profile", func(t *testing.T) {
		got, err := Resolve(Flags{}, envFunc(map[string]string{EnvVerifyTimeout: "90"}), profile)
		if err != nil {
			t.Fatal(err)
		}
		if got.VerifyTimeout != 90*time.Second {
			t.Errorf("VerifyTimeout = %s, want 90s from env (a bare number is seconds)", got.VerifyTimeout)
		}
	})

	t.Run("flag beats env and profile", func(t *testing.T) {
		got, err := Resolve(Flags{VerifyTimeout: durp(12 * time.Minute)},
			envFunc(map[string]string{EnvVerifyTimeout: "90"}), profile)
		if err != nil {
			t.Fatal(err)
		}
		if got.VerifyTimeout != 12*time.Minute {
			t.Errorf("VerifyTimeout = %s, want 12m from the flag", got.VerifyTimeout)
		}
	})
}
