package controller

import "strconv"

func itoa(n int) string   { return strconv.Itoa(n) }
func u64(n uint64) string { return strconv.FormatUint(n, 10) }
func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
func orDash(v string) string {
	if v == "" {
		return "-"
	}
	return v
}
