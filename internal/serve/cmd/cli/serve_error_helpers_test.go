package cli

import "strings"

func normalizedServeError(value string) string {
	return strings.Join(strings.Fields(value), "")
}
