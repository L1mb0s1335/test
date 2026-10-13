# p2pdrive — community-powered storage (MVP)

Google Drive-like storage where the disks belong to the community. Anyone can
donate space by running a `node`; anyone can store files. A central
`coordinator` keeps metadata, splits files into chunks, replicates each chunk
onto several nodes and repairs replication when nodes go offline.

    go build -o bin/ ./cmd/...
    ./bin/coordinator -listen :8080 -replication 2 -chunk-mb 4

    export DRIVE_SERVER=http://localhost:8080
    export DRIVE_TOKEN=$(./bin/drive register alice | sed 's/.*=//')

    # on any machine: donate 10 GiB
    ./bin/node -coordinator $DRIVE_SERVER -public http://<reachable-host>:9000 -dir ./data -capacity-mb 10240

    ./bin/drive upload photo.jpg
    ./bin/drive ls
    ./bin/drive get <id> out.jpg
    ./bin/drive rm <id>
    ./bin/drive nodes; ./bin/drive stats

`scripts/demo.sh` runs a full scenario (3 nodes, upload, kill a node, repair, download).

## Known MVP limits / next steps
- Coordinator relays all data (bandwidth bottleneck) and nodes must be directly reachable (no NAT traversal).
  Next: presigned URLs so clients talk to nodes directly; nodes behind NAT poll/long-connect to the coordinator.
- Chunks are stored unencrypted. Next: client-side encryption (donors must not read others' files).
- No fairness policy (e.g. quota proportional to donated space), no TLS, no per-user rate limits.
- Metadata is a single JSON file; swap for SQLite/Postgres. Orphan chunks on nodes that were offline during delete are not GC'd.
