// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/qifalab/euler-platform/services/witshield-engine/internal/controllercmd"
	"log"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"
var commit = "unknown"
var buildDate = "unknown"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Printf("witshield-controller %s (commit %s, built %s)\n", version, commit, buildDate)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := controllercmd.MainVersion(ctx, os.Args[1:], version); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}
