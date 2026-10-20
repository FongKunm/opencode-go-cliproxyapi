package thinking

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestSplitSuffix(t *testing.T) {
	cases := []struct {
		in, base, raw string
		ok            bool
	}{
		{"deepseek-v4-pro(high)", "deepseek-v4-pro", "high", true},
		{"opencode-go/deepseek-v4-pro(max)", "opencode-go/deepseek-v4-pro", "max", true},
		{"deepseek-v4-pro(8192)", "deepseek-v4-pro", "8192", true},
		{"m(a)(b)", "m(a)", "b", true},
		{"m()", "m", "", true},
		{"deepseek-v4-pro", "deepseek-v4-pro", "", false},
		{"m(high)x", "m(high)x", "", false},
		{"m)", "m)", "", false},
	}
	for _, c := range cases {
		base, raw, ok := SplitSuffix(c.in)
		if base != c.base || raw != c.raw || ok != c.ok {
			t.Errorf("SplitSuffix(%q) = (%q, %q, %v), want (%q, %q, %v)", c.in, base, raw, ok, c.base, c.raw, c.ok)
		}
	}
}

func TestSuffixOverride(t *testing.T) {
	deepseek := ForModel("deepseek-v4-pro", nil)
	declared := &pluginapi.ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh"}}
	cases := []struct {
		raw  string
		ts   *pluginapi.ThinkingSupport
		want Override
	}{
		{"high", deepseek, Override{Set: true, Effort: "high"}},
		{"MAX", deepseek, Override{Set: true, Effort: "max"}},
		{"xhigh", deepseek, Override{Set: true, Effort: "xhigh"}},
		{"minimal", deepseek, Override{Set: true, Effort: "minimal"}},
		{"none", deepseek, Override{Set: true, Effort: "none"}},
		{"0", deepseek, Override{Set: true, Effort: "none"}},
		{"512", deepseek, Override{Set: true, Effort: "minimal"}},
		{"8192", deepseek, Override{Set: true, Effort: "medium"}},
		{"08192", deepseek, Override{Set: true, Effort: "medium"}},
		{"31999", deepseek, Override{Set: true, Effort: "xhigh"}},
		{"auto", deepseek, Override{Set: true}},
		{"-1", deepseek, Override{Set: true}},
		{"max", nil, Override{Set: true, Effort: "high"}},
		{"max", declared, Override{Set: true, Effort: "xhigh"}},
		{"none", nil, Override{Set: true, Effort: "low"}},
		{"31999", nil, Override{Set: true, Effort: "high"}},
		{"foo", deepseek, Override{}},
		{"", deepseek, Override{}},
		{"-5", deepseek, Override{}},
		{"1.5", deepseek, Override{}},
	}
	for _, c := range cases {
		if got := SuffixOverride(c.raw, c.ts); got != c.want {
			t.Errorf("SuffixOverride(%q) = %+v, want %+v", c.raw, got, c.want)
		}
	}
}
