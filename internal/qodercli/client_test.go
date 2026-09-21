package qodercli

import "testing"

func TestClampTierStopsAtTheStrongestTierTheModelOffers(t *testing.T) {
	// Measured shapes: Qwen3.8-Flash has no high/max rung, GLM-5.3 has no
	// medium/xhigh rung. Asking for a missing tier must never return it, because
	// the CLI would silently run the turn with reasoning_effort=none.
	cases := []struct {
		name   string
		ladder []string
		in     string
		want   string
	}{
		{"qwen keeps its own tiers", []string{"none", "low", "medium", "xhigh"}, "low", "low"},
		{"qwen steps high down to medium", []string{"none", "low", "medium", "xhigh"}, "high", "medium"},
		{"qwen caps max at its top", []string{"none", "low", "medium", "xhigh"}, "max", "xhigh"},
		{"glm keeps its top tier", []string{"none", "low", "high", "max"}, "max", "max"},
		{"glm steps xhigh down to high", []string{"none", "low", "high", "max"}, "xhigh", "high"},
		{"glm steps medium down to low", []string{"none", "low", "high", "max"}, "medium", "low"},
		{"unknown tier passes through", []string{"none", "low"}, "banana", "banana"},
		{"measured tier-less model sends nothing", nil, "xhigh", ""},
		// Auto exposes no thinking tier at all: forcing none would switch its
		// default thinking off instead of picking a level.
		{"tier-less model omits instead of forcing none", []string{"none"}, "medium", ""},
		{"explicit none still reaches a tier-less model", []string{"none"}, "none", "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampTier(tc.ladder, tc.in); got != tc.want {
				t.Fatalf("clampTier(%v, %q) = %q, want %q", tc.ladder, tc.in, got, tc.want)
			}
		})
	}
}

func TestErrorLinesDropRuntimeLogNoise(t *testing.T) {
	// Both shapes were observed standing in for the failure message, so the user
	// read a crashpad path or a skill collision instead of what went wrong.
	stderr := "[0920/043244.997:ERROR:third_party\\crashpad\\crashpad\\util\\win\\registration_protocol_win.cc:108] CreateFile: 系统找不到指定的文件。 (0x2)\n" +
		"[0920/043244.998:FATAL:gin\\v8_initializer.cc:681] Error loading V8 startup snapshot file\n" +
		"Skill \"paseo-advisor\" overrides same-source skill at /x/y\n" +
		"Warning: native tool hooks are not fully supported\n" +
		"upstream rejected the prompt"
	if got := errorLines(stderr, 6); got != "upstream rejected the prompt" {
		t.Fatalf("errorLines kept noise: %q", got)
	}

	// Nothing but noise must read as "no reason given", not as the noise itself.
	if got := errorLines("[0920/043224.168:FATAL:gin\\v8_initializer.cc:681] boom\n", 6); got != "" {
		t.Fatalf("pure noise should yield empty, got %q", got)
	}

	// A real failure that happens to mention a log-looking line still surfaces.
	if got := errorLines("gateway returned 403 for model Qwen3.8-Flash", 6); got == "" {
		t.Fatal("plain error dropped")
	}
}

func TestRejectedCredentialFindsTheCauseAboveTheTail(t *testing.T) {
	// The measured shape: the gateway's reason arrives first, then six frames of
	// obfuscated promise chain, so the tail alone never mentions the token.
	stderr := "auth.getUserInfo failed: token is not active\n" +
		"    at async xoe.initAuthWithOptions (file:///runtime.obf.mjs:1:3958178)\n" +
		"    at async xoe.initAuth (file:///runtime.obf.mjs:1:3955357)\n" +
		"    at async C$.refreshAuth (file:///runtime.obf.mjs:267:819330)\n"
	if got := rejectedCredential(stderr); got == "" {
		t.Fatal("a rejected credential must be named, not reported as a stack")
	}
	// The same prefix fronts plain network failures, which have to stay retryable.
	if got := rejectedCredential("auth.getUserInfo failed: connection reset"); got != "" {
		t.Fatalf("a network hiccup is not a rejected token, matched %q", got)
	}
}

// TestParseResultDistinguishesFinishedFromKilled: a CLI killed mid-turn leaves
// assistant frames but never a result frame. Chat uses that flag to refuse to
// report a half answer as a completed turn, which is what surfaced as replies
// that simply stopped mid-sentence with no error.
func TestParseResultDistinguishesFinishedFromKilled(t *testing.T) {
	partial := `{"type":"assistant","message":{"content":[{"type":"text","text":"说到一半"}]}}` + "\n"

	result, sawResult, err := parseResult(partial, "Qwen3.8-Flash", "CN", SiteCN, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Text != "说到一半" {
		t.Fatalf("partial text must still parse, got %#v", result)
	}
	if sawResult {
		t.Fatal("frames without a result frame cannot prove the turn finished")
	}

	finished := partial + `{"type":"result","subtype":"success","result":"说到一半就说完了"}` + "\n"
	result, sawResult, err = parseResult(finished, "Qwen3.8-Flash", "CN", SiteCN, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !sawResult {
		t.Fatal("a success result frame is the evidence that the turn finished")
	}
	if result.Text != "说到一半就说完了" {
		t.Fatalf("text = %q", result.Text)
	}
}
