// wsprobe is a tiny manual smoke-test for rpc.TipNotifier: dials a node,
// subscribes to NewBlock, prints the heights it sees for ~20s, and exits.
// Intentionally small; the real ingest path uses the notifier directly.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"sync-state/internal/rpc"
)

func main() {
	endpoint := flag.String("rpc", "https://public.testnet.structs.network:26657", "CometBFT RPC URL")
	d := flag.Duration("for", 20*time.Second, "how long to listen")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *d+5*time.Second)
	defer cancel()

	n := rpc.NewTipNotifier(
		[]string{*endpoint},
		log.New(os.Stderr, "wsprobe: ", log.LstdFlags|log.Lmicroseconds),
	)
	go n.Run(ctx)

	deadline := time.NewTimer(*d)
	defer deadline.Stop()
	count := 0
	for {
		select {
		case <-deadline.C:
			fmt.Printf("result: received %d NewBlock events in %s; connected=%v\n", count, *d, n.Connected())
			return
		case h := <-n.C():
			count++
			fmt.Printf("got NewBlock h=%d at %s\n", h, time.Now().Format("15:04:05.000"))
		case <-ctx.Done():
			fmt.Println("context cancelled")
			return
		}
	}
}
