package kit

import "strings"

// SplitCmdArg splits a slash query into the command token and the trailing
// argument: "/model gpt-4o" → ("model", "gpt-4o"). Leading slash and surrounding
// whitespace are trimmed; with no space the whole token is the command.
func SplitCmdArg(s string) (cmd, arg string) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "/"))
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], strings.TrimSpace(s[i+1:])
	}
	return s, ""
}
