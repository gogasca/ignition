package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// emit writes v as indented JSON when -o json is set. It returns true when it
// handled output, so callers can `if e.emit(x) { return nil }` before their
// text rendering.
func (e *env) emit(v any) bool {
	if !e.g.json() {
		return false
	}
	enc := json.NewEncoder(e.stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	return true
}

func (e *env) printf(format string, a ...any) {
	fmt.Fprintf(e.stdout, format, a...)
}

// table renders rows with a header using elastic tab stops.
func table(w io.Writer, header []string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	_ = tw.Flush()
}

func ageOf(ts string) string {
	if ts == "" {
		return "-"
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
