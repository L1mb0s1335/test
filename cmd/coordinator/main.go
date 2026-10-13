// Command coordinator is the central brain of the community drive. It keeps
// metadata (users, nodes, files, chunk placement), splits uploads into chunks,
// spreads replicas over online nodes, and re-replicates when nodes vanish.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"
)

func main() {
	listen := flag.String("listen", ":8080", "listen address")
	state := flag.String("state", "coordinator-state.json", "metadata file")
	repl := flag.Int("replication", 2, "replicas per chunk")
	chunk := flag.Int("chunk-mb", 4, "chunk size in MiB")
	offline := flag.Duration("offline-after", 20*time.Second, "mark node offline after this long without heartbeat")
	flag.Parse()

	c := newCoordinator(*state, *repl, *chunk<<20, *offline)
	go c.repairLoop(10 * time.Second)

	log.Printf("coordinator on %s (replication=%d, chunk=%dMiB)", *listen, *repl, *chunk)
	log.Fatal(http.ListenAndServe(*listen, c.routes()))
}
