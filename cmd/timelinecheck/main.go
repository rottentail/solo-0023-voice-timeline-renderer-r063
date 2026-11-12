package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"voice-timeline-renderer/internal/checkflow"
)

func main() {
	workflow := flag.String("workflow", "", "workflow identifier to execute")
	candidates := flag.Int("candidates", 96, "number of bounded projects for the fuzz-audit workflow")
	workers := flag.Int("workers", 2, "parallel isolated workers for the fuzz-audit workflow")
	seed := flag.Uint64("seed", 20261112, "deterministic PRNG seed for the fuzz-audit workflow")
	artifactDir := flag.String("artifact-dir", "./var/audit-artifacts", "directory for minimized replay artifacts")
	artifact := flag.String("artifact", "", "path to manifest.json for the replay-artifact workflow")
	flag.Parse()
	if *workflow == "" {
		fmt.Fprintln(os.Stderr, "-workflow is required")
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch *workflow {
	case "fuzz-audit":
		err = checkflow.RunAuditWorkflow(ctx, *candidates, *workers, *seed, *artifactDir)
	case "replay-artifact":
		if *artifact == "" {
			fmt.Fprintln(os.Stderr, "-artifact is required for replay-artifact")
			os.Exit(2)
		}
		err = checkflow.RunReplayWorkflow(ctx, *artifact)
	default:
		err = checkflow.Run(ctx, *workflow)
	}
	if err != nil {
		var violation checkflow.ErrViolationFound
		if asViolation(err, &violation) {
			// A found and minimized violation is an audit finding, not a
			// harness crash: exit 2 so CI can distinguish it from setup errors.
			fmt.Fprintf(os.Stderr, "workflow %s found a minimized violation: %v\n", *workflow, err)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "workflow %s failed: %v\n", *workflow, err)
		os.Exit(1)
	}
	fmt.Printf("workflow %s passed\n", *workflow)
}

func asViolation(err error, target *checkflow.ErrViolationFound) bool {
	found, ok := err.(checkflow.ErrViolationFound)
	if !ok {
		return false
	}
	*target = found
	return true
}
