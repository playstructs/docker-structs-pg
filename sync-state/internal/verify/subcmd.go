package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"sync-state/internal/db"
	"sync-state/internal/rpc"
)

// CmdInputs is what the subcommand wrapper hands the runner. Mirrors the
// runtime dependencies a Config wire-up would otherwise build inline.
type CmdInputs struct {
	Pool      db.Querier
	RPC       *rpc.Client
	ChainID   string
	MirrorRaw bool

	// Mirrored knobs from sync.Config so verify owns its own behaviour
	// without depending on the sync package (avoids an import cycle).
	ErrorsOnly  bool
	WriteReport bool
	JSON        bool
	LagWarn     int64
}

// Run executes the verify subcommand. Returns the process exit code:
// 0 = clean (all PASS/INFO/SKIP), 1 = at least one FAIL.
func Run(ctx context.Context, in CmdInputs, stdout, stderr io.Writer) int {
	lag := in.LagWarn
	if lag <= 0 {
		lag = 5
	}
	results, err := RunChecks(ctx, Inputs{
		Pool:      in.Pool,
		RPC:       in.RPC,
		ChainID:   in.ChainID,
		MirrorRaw: in.MirrorRaw,
		LagWarn:   lag,
	}, Options{
		ErrorsOnly:  in.ErrorsOnly,
		WriteReport: in.WriteReport,
	})
	if err != nil {
		fmt.Fprintf(stderr, "verify: %v\n", err)
		return 1
	}

	if in.JSON {
		emitJSON(stdout, results)
	} else {
		emitText(stdout, results)
	}

	for _, r := range results {
		if r.Status == StatusFail {
			return 1
		}
	}
	return 0
}

// RunChecks is the externally callable runner. Tests call it directly with
// a fixture-loaded pool and no RPC.
func RunChecks(ctx context.Context, in Inputs, opts Options) ([]CheckResult, error) {
	return runImpl(ctx, in, opts)
}

// emitText renders the human-readable summary on stdout. One block per
// check; FAILs are prefixed with the status code so they're easy to grep.
func emitText(w io.Writer, results []CheckResult) {
	var pass, fail, info, skip int
	for _, r := range results {
		fmt.Fprintf(w, "%-32s %-4s  %s\n", r.Name, r.Status, r.Detail)
		if len(r.Counts) > 0 {
			keys := make([]string, 0, len(r.Counts))
			for k := range r.Counts {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				fmt.Fprintf(w, "    %s = %v\n", k, r.Counts[k])
			}
		}
		switch r.Status {
		case StatusPass:
			pass++
		case StatusFail:
			fail++
		case StatusInfo:
			info++
		case StatusSkip:
			skip++
		}
	}
	fmt.Fprintf(w, "\nSummary: %d PASS, %d FAIL, %d INFO, %d SKIP\n", pass, fail, info, skip)
}

func emitJSON(w io.Writer, results []CheckResult) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(results)
}
