package ui

import "strconv"

// Count is n with the noun for it: "1 key", "2 keys". many is the plural,
// or "" for one+"s".
func Count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	if many == "" {
		many = one + "s"
	}
	return strconv.Itoa(n) + " " + many
}
