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
	offline := flag.Duration("offline-after", 30*time.Second, "mark node offline after this long without heartbeat")
	tlsCert := flag.String("tls-cert", "", "TLS certificate (enables HTTPS; strongly recommended on the internet)")
	tlsKey := flag.String("tls-key", "", "TLS key")
	flag.Parse()

	c := newCoordinator(*state, *repl, *chunk<<20, *offline)
	go c.repairLoop(10 * time.Second)

	log.Printf("coordinator on %s (replication=%d, chunk=%dMiB)", *listen, *repl, *chunk)
	srv := &http.Server{Addr: *listen, Handler: c.routes(), ReadHeaderTimeout: 10 * time.Second}
	if *tlsCert != "" {
		log.Fatal(srv.ListenAndServeTLS(*tlsCert, *tlsKey))
	}
	log.Println("WARNING: serving plain HTTP; tokens travel unencrypted (file contents are always end-to-end encrypted)")
	log.Fatal(srv.ListenAndServe())
}
