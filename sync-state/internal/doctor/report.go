package doctor

import (
	"fmt"
	"io"
	"strings"
)

// Print writes a human-friendly version of the report to w. The format is
// intentionally one line per check so it greps cleanly. With multiple
// RPC endpoints, each endpoint gets its own indented sub-section so
// operators can see which host is causing trouble at a glance.
func (r *Report) Print(w io.Writer) {
	fmt.Fprintf(w, "Node compatibility check (preferred=%s):\n", r.RPCURL)
	const nameWidth = 26

	if len(r.Endpoints) > 0 {
		fmt.Fprintln(w, "  RPC pool:")
		for _, ep := range r.Endpoints {
			fmt.Fprintf(w, "    %-8s %s\n", ep.Role, ep.URL)
			for _, c := range ep.Checks {
				detail := strings.ReplaceAll(c.Detail, "\n", "\n        ")
				fmt.Fprintf(w, "      %-*s %-5s %s\n", nameWidth, c.Name, c.Severity, detail)
			}
		}
	}

	for _, c := range r.Checks {
		// Multi-line details get indented after the first line so the
		// alignment stays readable.
		detail := strings.ReplaceAll(c.Detail, "\n", "\n      ")
		fmt.Fprintf(w, "  %-*s %-5s %s\n", nameWidth, c.Name, c.Severity, detail)
	}
	fmt.Fprintf(w, "  Verdict: %s\n", r.Verdict)
}
