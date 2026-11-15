package domain

import "testing"

func FuzzParseMatchers(f *testing.F) {
	for _, s := range []string{"source=k8s* severity=warning|info", "a=!b", "=", "x=(", "status=firing heartbeat=wd"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		m, err := ParseMatchers(s)
		if err != nil {
			return
		}
		c, err := CompileMatch(m)
		if err != nil {
			t.Fatalf("parsed but does not compile: %q", s)
		}
		c.Explain(&Event{Source: "x", Labels: map[string]string{"a": "b"}})
		if again, err := ParseMatchers(FormatMatchers(m)); err != nil || FormatMatchers(again) != FormatMatchers(m) {
			t.Fatalf("format does not round-trip: %q", s)
		}
	})
}

func FuzzParseDuration(f *testing.F) {
	for _, s := range []string{"1d12h", "90m", "0s", "1.5h", "d", "99999999d"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if d, err := ParseDuration(s); err == nil && d <= 0 {
			t.Fatalf("%q gave %v", s, d)
		}
	})
}

func FuzzLookup(f *testing.F) {
	f.Add("a.b.0.c")
	f.Add("")
	f.Add("..")
	f.Fuzz(func(t *testing.T, p string) {
		Lookup(map[string]any{"a": map[string]any{"b": []any{map[string]any{"c": 1}}}}, p)
	})
}
