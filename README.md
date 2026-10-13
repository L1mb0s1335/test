# p2pdrive — community storage with end-to-end encryption

A Google Drive-like service where the disks belong to the community. Anyone
can donate disk space by running the app; anyone can store files. A central
**coordinator** keeps the directory and places replicas; the **app**
(Windows / Linux) encrypts everything on your machine first, so **nobody but
you can read your files** — not the coordinator, not the donors.

    p2pdrive           desktop app: local UI opens in your browser
    coordinator        server, run once by whoever hosts the community

## Quick start

    ./build.sh                                  # dist/ has Linux + Windows binaries

    # operator (any server; use TLS on the internet)
    ./dist/coordinator-linux-amd64 -listen :8443 -tls-cert cert.pem -tls-key key.pem

    # every user: run the app, it opens the UI
    ./dist/p2pdrive-linux-amd64                  # or p2pdrive-windows-amd64.exe

In the UI: connect to the coordinator → create an account → choose a
passphrase → upload files. On the **Donate** tab pick a folder and how much
space to give. Donating needs no open ports (nodes only connect outwards).
Headless donor: `p2pdrive node`. CLI: `p2pdrive ls|put|get|rm` (needs
`DRIVE_PASSPHRASE`). `scripts/demo.sh` runs a full end-to-end test.

## Security model

| Who | Sees |
|---|---|
| You | everything |
| Coordinator | account name, random ids, ciphertext sizes, upload times, which nodes hold which chunk |
| Donors | opaque encrypted chunks with random ids |

- Passphrase → **Argon2id** (64 MiB, t=3) → master key; kept only in memory.
- Every file gets a random 256-bit key; content is encrypted with
  **AES-256-GCM in 1 MiB segments (STREAM construction)**: reordering, dropping,
  truncating or modifying any segment is detected on download. Plus a SHA-256
  per stored chunk. File names/sizes live in a metadata blob encrypted under the
  master key.
- Donors or the coordinator can **withhold or delete** data (replication + repair
  mitigates this) but cannot read or undetectably alter it.
- The local UI listens on 127.0.0.1 only, behind a random per-run access key,
  Host-header check and anti-CSRF header.

**Limits you should know**
- **Lose the passphrase = lose the data.** No recovery, no passphrase change yet. Use a long one.
- The account token (stored in the config file) lets someone delete your files or
  fill your quota, but not read anything.
- Use TLS for the coordinator: otherwise tokens cross the network in clear (file contents never do).
- Not yet: file sharing between users, folders, quotas/fairness (e.g. space
  proportional to what you donate), rollback detection, multiple coordinators.
- The coordinator still relays all data (bandwidth bottleneck) and keeps metadata
  in one JSON file — fine for an MVP/small community.
