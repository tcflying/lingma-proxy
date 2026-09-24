package qodercli

import (
	"testing"
	"time"
)

// TestDecodeJobCredentialAlwaysKnowsWhenToRenew is C5: with neither expires_at nor
// expires_in the credential kept the zero time, expiresIn read that as "immortal",
// and the minted job token was never renewed -- once the gateway dropped it every
// request failed until the proxy restarted.
func TestDecodeJobCredentialAlwaysKnowsWhenToRenew(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"no expiry fields at all", `{"token":"t"}`},
		{"expires_at the parser does not know", `{"token":"t","expires_at":"1755129600000"}`},
		{"expires_at empty and expires_in absent", `{"token":"t","expires_at":"","expires_in":0}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cred, err := decodeJobCredential([]byte(tc.payload))
			if err != nil {
				t.Fatal(err)
			}
			if cred.ExpiresAt.IsZero() {
				t.Fatal("a credential with no parseable expiry must fall back to a bounded lifetime")
			}
			remaining := time.Until(cred.ExpiresAt)
			if remaining <= 9*time.Minute || remaining > 11*time.Minute {
				t.Fatalf("fallback lifetime = %s, want the bounded 10 minutes", remaining)
			}
			// The whole point: a margin wider than the assumed lifetime renews it.
			if !cred.expiresIn(defaultJobTokenLifetime + time.Minute) {
				t.Fatal("the fallback credential has to be inside the renewal window")
			}
		})
	}

	t.Run("an absolute expires_at is kept", func(t *testing.T) {
		const stamp = "2030-01-01T00:00:00Z"
		cred, err := decodeJobCredential([]byte(`{"token":"t","expires_at":"` + stamp + `"}`))
		if err != nil {
			t.Fatal(err)
		}
		want, _ := time.Parse(time.RFC3339, stamp)
		if !cred.ExpiresAt.Equal(want) {
			t.Fatalf("expires_at = %s, want %s", cred.ExpiresAt, want)
		}
	})

	// Measured shape for this gateway is the RFC3339 expires_at above; expires_in
	// only ever appeared alongside it. The millisecond reading is therefore kept
	// verbatim and pinned here, so a future change to the unit is a deliberate one.
	t.Run("expires_in still counts milliseconds", func(t *testing.T) {
		cred, err := decodeJobCredential([]byte(`{"token":"t","expires_in":3600000}`))
		if err != nil {
			t.Fatal(err)
		}
		if remaining := time.Until(cred.ExpiresAt); remaining <= 59*time.Minute || remaining > time.Hour {
			t.Fatalf("expires_in=3600000 yielded %s, want one hour", remaining)
		}
	})
}
