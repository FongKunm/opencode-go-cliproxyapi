package thinking

import (
	"slices"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestIsDeepSeek(t *testing.T) {
	for model, want := range map[string]bool{
		"deepseek-v4-pro":        true,
		"deepseek-v4.1-flash":    true,
		"DeepSeek-R2":            true,
		" deepseek-chat":         true,
		"kimi-k2":                false,
		"glm-5.3":                false,
		"opencode-go/deepseek-x": false, // upstream IDs carry no public prefix
		"":                       false,
	} {
		if got := IsDeepSeek(model); got != want {
			t.Errorf("IsDeepSeek(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestForModelDeepSeekFullLadder(t *testing.T) {
	got := ForModel("deepseek-v4-pro", nil)
	if got == nil || !slices.Equal(SupportedLevels(got), CanonicalLevels) || !got.ZeroAllowed {
		t.Fatalf("deepseek nil capability = %+v, want full ladder with ZeroAllowed", got)
	}

	declared := &pluginapi.ThinkingSupport{Min: 128, Max: 4096, DynamicAllowed: true, Levels: []string{"low", "high"}}
	got = ForModel("DeepSeek-V4.1-Flash", declared)
	if !slices.Equal(SupportedLevels(got), CanonicalLevels) || !got.ZeroAllowed ||
		got.Min != 128 || got.Max != 4096 || !got.DynamicAllowed {
		t.Fatalf("deepseek declared capability = %+v, want full ladder keeping Min/Max/Dynamic", got)
	}
	if !slices.Equal(declared.Levels, []string{"low", "high"}) || declared.ZeroAllowed {
		t.Fatalf("ForModel mutated the catalog record: %+v", declared)
	}
	got.Levels[0] = "mutated"
	if CanonicalLevels[0] != "none" {
		t.Fatal("ForModel must not alias CanonicalLevels")
	}
}

func TestForModelOtherFamiliesUnchanged(t *testing.T) {
	ts := &pluginapi.ThinkingSupport{Levels: []string{"low", "medium", "high"}}
	if got := ForModel("kimi-k2", ts); got != ts {
		t.Fatalf("non-deepseek capability must pass through unchanged, got %+v", got)
	}
	if got := ForModel("glm-5.3", nil); got != nil {
		t.Fatalf("non-deepseek nil capability must stay nil, got %+v", got)
	}
}

func TestClamp(t *testing.T) {
	deepseek := ForModel("deepseek-v4-pro", nil)
	zeroOnly := &pluginapi.ThinkingSupport{ZeroAllowed: true, Levels: []string{"low", "high"}}
	cases := []struct {
		name   string
		effort string
		ts     *pluginapi.ThinkingSupport
		want   string
		ok     bool
	}{
		{"deepseek max", "max", deepseek, "max", true},
		{"deepseek none", "none", deepseek, "none", true},
		{"deepseek minimal", "minimal", deepseek, "minimal", true},
		{"deepseek xhigh mixed case", " XHigh ", deepseek, "xhigh", true},
		{"default ladder caps max", "max", nil, "high", true},
		{"default ladder caps xhigh", "xhigh", nil, "high", true},
		{"default ladder lifts minimal", "minimal", nil, "low", true},
		{"default ladder lifts none", "none", nil, "low", true},
		{"zero allowed keeps none undeclared", "none", zeroOnly, "none", true},
		{"declared ladder nearest weaker on tie", "medium", zeroOnly, "low", true},
		{"auto is not a level", "auto", deepseek, "", false},
		{"unknown value", "ultra", deepseek, "", false},
		{"empty", "", deepseek, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := Clamp(c.effort, c.ts)
			if got != c.want || ok != c.ok {
				t.Fatalf("Clamp(%q) = (%q, %v), want (%q, %v)", c.effort, got, ok, c.want, c.ok)
			}
		})
	}
}

func TestClaudeEffort(t *testing.T) {
	deepseek := ForModel("deepseek-v4-pro", nil)
	cases := []struct {
		name         string
		thinkingType string
		budget       int64
		outputEffort string
		ts           *pluginapi.ThinkingSupport
		want         string
		ok           bool
	}{
		{"adaptive with effort max on deepseek", "adaptive", 0, "max", deepseek, "max", true},
		{"adaptive with effort max clamps on default ladder", "adaptive", 0, "max", nil, "high", true},
		{"adaptive with effort low", "adaptive", 0, "low", deepseek, "low", true},
		{"adaptive without effort omits", "adaptive", 0, "", deepseek, "", false},
		{"no thinking control without effort omits", "", 0, "", deepseek, "", false},
		{"no thinking control with effort", "", 0, "medium", deepseek, "medium", true},
		{"disabled on deepseek is none", "disabled", 0, "", deepseek, "none", true},
		{"disabled outranks an effort hint", "disabled", 0, "high", deepseek, "none", true},
		{"disabled on default ladder clamps to weakest", "disabled", 0, "", nil, "low", true},
		{"enabled budget keeps the threshold table", "enabled", 8192, "", deepseek, "medium", true},
		{"enabled large budget reaches xhigh on deepseek", "enabled", 31999, "", deepseek, "xhigh", true},
		{"enabled large budget caps at high on default ladder", "enabled", 31999, "", nil, "high", true},
		{"effort outranks the budget", "enabled", 1024, "max", deepseek, "max", true},
		{"unknown effort falls back to the budget", "enabled", 1024, "ultra", deepseek, "low", true},
		{"unknown effort with adaptive omits", "adaptive", 0, "ultra", deepseek, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ClaudeEffort(c.thinkingType, c.budget, c.outputEffort, c.ts)
			if got != c.want || ok != c.ok {
				t.Fatalf("ClaudeEffort(%q, %d, %q) = (%q, %v), want (%q, %v)",
					c.thinkingType, c.budget, c.outputEffort, got, ok, c.want, c.ok)
			}
		})
	}
}
