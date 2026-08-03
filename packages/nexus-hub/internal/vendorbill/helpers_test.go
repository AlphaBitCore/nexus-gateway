package vendorbill

import "strconv"

// itoa renders an int64 for inline JSON fixtures.
func itoa(v int64) string { return strconv.FormatInt(v, 10) }
