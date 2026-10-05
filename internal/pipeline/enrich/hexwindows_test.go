package enrich

import (
	"testing"
)

func TestHexWindowsMatchesSubstringSearch(t *testing.T) {
	hash := "aabbccddeeff00112233445566778899aabbccdd"
	urls := map[string]bool{
		"https://github.com/u/r/commit/" + hash:                  true, // exact 40-hex run
		"https://x.dev/deadbeef":                                 true, // run shorter than 40: no window
		"https://y.dev/00" + hash + "ff":                         true, // hash embedded mid-run: only windowing finds it
		"https://z.dev/AABBCCDDEEFF00112233445566778899aabbccdd": true, // uppercase never matched the lowercased markers before either
	}
	windows := hexWindows(urls, 40)
	if !windows[hash] {
		t.Fatal("exact commit-url hash not found")
	}
	if !windows["00"+hash[:38]] {
		t.Fatal("embedded window not found; windowing must cover every offset")
	}
	if windows["deadbeef"] {
		t.Fatal("short runs must not produce windows")
	}
}
