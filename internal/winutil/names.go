package winutil

import "strings"

func normalizeExeName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	return strings.TrimSuffix(name, ".exe")
}
