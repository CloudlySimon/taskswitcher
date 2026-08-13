package winutil

import "testing"

func TestNormalizeExeName(t *testing.T) {
	tests := map[string]string{
		"msedge":         "msedge",
		"MSEDGE.EXE":     "msedge",
		"  elwa.exe  ":   "elwa",
		"CrestronXPanel": "crestronxpanel",
	}
	for input, want := range tests {
		if got := normalizeExeName(input); got != want {
			t.Errorf("normalizeExeName(%q) = %q, want %q", input, got, want)
		}
	}
}
