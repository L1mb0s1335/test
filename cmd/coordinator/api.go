package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"p2pdrive/internal/proto"
)

func (c *coordinator) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/users", c.registerUser)
	mux.HandleFunc("GET /api/me", c.me)
	mux.HandleFunc("PUT /api/keyinfo", c.setKeyInfo)
	mux.HandleFunc("POST /api/nodes/register", c.registerNode)
	mux.HandleFunc("GET /api/nodes/poll", c.pollJob)
	mux.HandleFunc("POST /api/nodes/result", c.jobResult)
	mux.HandleFunc("GET /api/nodes", c.listNodes)
	mux.HandleFunc("GET /api/stats", c.stats)
	mux.HandleFunc("POST /api/files", c.upload)
	mux.HandleFunc("GET /api/files", c.listFiles)
	mux.HandleFunc("GET /api/files/{id}", c.download)
	mux.HandleFunc("DELETE /api/files/{id}", c.deleteFile)
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (c *coordinator) auth(r *http.Request) *User {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snap.Users[tok]
}

// authNode checks node id + key and refreshes its liveness.
func (c *coordinator) authNode(r *http.Request) *Node {
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.snap.Nodes[r.URL.Query().Get("node")]
	if n == nil || subtle.ConstantTimeCompare([]byte(n.Key), []byte(key)) != 1 {
		return nil
	}
	n.LastSeen = time.Now()
	return n
}

func (c *coordinator) touch(nodeID string) {
	c.mu.Lock()
	if n := c.snap.Nodes[nodeID]; n != nil {
		n.LastSeen = time.Now()
	}
	c.mu.Unlock()
}

func (c *coordinator) registerUser(w http.ResponseWriter, r *http.Request) {
	var req proto.RegisterUserReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	u := &User{ID: randID(8), Name: req.Name, Token: randID(24)}
	c.mu.Lock()
	c.snap.Users[u.Token] = u
	c.saveLocked()
	c.mu.Unlock()
	writeJSON(w, proto.RegisterUserResp{UserID: u.ID, Token: u.Token})
}

func (c *coordinator) me(w http.ResponseWriter, r *http.Request) {
	u := c.auth(r)
	if u == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	writeJSON(w, proto.Me{UserID: u.ID, Name: u.Name, Salt: u.Salt, Check: u.Check})
}

// setKeyInfo stores the public KDF salt + passphrase check, once.
func (c *coordinator) setKeyInfo(w http.ResponseWriter, r *http.Request) {
	u := c.auth(r)
	if u == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var ki proto.KeyInfo
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&ki); err != nil || len(ki.Salt) == 0 || len(ki.Check) == 0 {
		http.Error(w, "salt and check required", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if u.Salt != nil {
		http.Error(w, "key info already set", http.StatusConflict)
		return
	}
	u.Salt, u.Check = ki.Salt, ki.Check
	c.saveLocked()
	w.WriteHeader(http.StatusNoContent)
}

func (c *coordinator) registerNode(w http.ResponseWriter, r *http.Request) {
	u := c.auth(r)
	if u == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req proto.RegisterNodeReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.Capacity <= 0 {
		http.Error(w, "positive capacity required", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.snap.Nodes[req.NodeID]
	if n != nil { // re-registration after restart
		if n.Owner != u.ID || subtle.ConstantTimeCompare([]byte(n.Key), []byte(req.NodeKey)) != 1 {
			http.Error(w, "node identity mismatch", http.StatusForbidden)
			return
		}
	} else {
		n = &Node{ID: randID(8), Owner: u.ID, Key: randID(24)}
		c.snap.Nodes[n.ID] = n
	}
	n.Capacity, n.LastSeen = req.Capacity, time.Now()
	c.saveLocked()
	log.Printf("node %s registered: %d MiB", n.ID, n.Capacity>>20)
	writeJSON(w, proto.RegisterNodeResp{NodeID: n.ID, NodeKey: n.Key})
}

// pollJob is the node's long-poll: it blocks up to 5s for work. Liveness is
// refreshed at the start and end of each poll, so -offline-after must be > 5s.
func (c *coordinator) pollJob(w http.ResponseWriter, r *http.Request) {
	n := c.authNode(r)
	if n == nil {
		http.Error(w, "unknown node", http.StatusForbidden)
		return
	}
	defer c.touch(n.ID)
	select {
	case j := <-c.relay.queue(n.ID):
		w.Header().Set(proto.HdrJobID, j.id)
		w.Header().Set(proto.HdrOp, j.op)
		w.Header().Set(proto.HdrChunkID, j.chunkID)
		w.Write(j.body)
	case <-time.After(5 * time.Second):
		w.WriteHeader(http.StatusNoContent)
	case <-r.Context().Done():
	}
}

func (c *coordinator) jobResult(w http.ResponseWriter, r *http.Request) {
	n := c.authNode(r)
	if n == nil {
		http.Error(w, "unknown node", http.StatusForbidden)
		return
	}
	status, _ := strconv.Atoi(r.URL.Query().Get("status"))
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(c.chunkSize)+1024))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c.relay.mu.Lock()
	j := c.relay.jobs[r.URL.Query().Get("job")]
	c.relay.mu.Unlock()
	if j == nil || j.node != n.ID {
		http.Error(w, "unknown job", http.StatusNotFound)
		return
	}
	select {
	case j.done <- jobResult{status: status, body: body}:
	default:
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *coordinator) listNodes(w http.ResponseWriter, r *http.Request) {
	u := c.auth(r)
	if u == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c.mu.Lock()
	out := []proto.NodeInfo{}
	for _, n := range c.snap.Nodes {
		out = append(out, proto.NodeInfo{ID: n.ID, Mine: n.Owner == u.ID, Capacity: n.Capacity,
			Used: n.Used, Online: c.onlineLocked(n), LastSeen: n.LastSeen})
	}
	c.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, out)
}

func (c *coordinator) stats(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := proto.Stats{Users: len(c.snap.Users), NodesTotal: len(c.snap.Nodes), Files: len(c.snap.Files), Replication: c.repl}
	for _, n := range c.snap.Nodes {
		if c.onlineLocked(n) {
			s.NodesOnline++
			s.Capacity += n.Capacity
			s.Used += n.Used
		}
	}
	writeJSON(w, s)
}

func (c *coordinator) listFiles(w http.ResponseWriter, r *http.Request) {
	u := c.auth(r)
	if u == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c.mu.Lock()
	out := []proto.FileInfo{}
	for _, f := range c.snap.Files {
		if f.Owner == u.ID {
			out = append(out, proto.FileInfo{ID: f.ID, Meta: f.Meta, Size: f.Size, Chunks: len(f.Chunks), Created: f.Created})
		}
	}
	c.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	writeJSON(w, out)
}

// upload streams the request body, cutting it into chunks and placing each
// chunk on c.repl distinct nodes before reading the next one.
func (c *coordinator) upload(w http.ResponseWriter, r *http.Request) {
	u := c.auth(r)
	if u == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	meta, err := base64.RawURLEncoding.DecodeString(r.URL.Query().Get("meta"))
	if err != nil || len(meta) == 0 || len(meta) > 4096 {
		http.Error(w, "?meta= (base64url encrypted metadata) required", http.StatusBadRequest)
		return
	}
	f := &File{ID: randID(8), Owner: u.ID, Meta: meta, Created: time.Now()}
	buf := make([]byte, c.chunkSize)
	for i := 0; ; i++ {
		n, err := io.ReadFull(r.Body, buf)
		if n > 0 {
			ch, perr := c.placeChunk(f.ID, i, buf[:n])
			if perr != nil {
				c.dropChunks(f.Chunks)
				http.Error(w, perr.Error(), http.StatusInsufficientStorage)
				return
			}
			f.Chunks = append(f.Chunks, ch)
			f.Size += int64(n)
		}
		if err == io.EOF || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			c.dropChunks(f.Chunks)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	c.mu.Lock()
	c.snap.Files[f.ID] = f
	c.saveLocked()
	c.mu.Unlock()
	writeJSON(w, proto.FileInfo{ID: f.ID, Meta: f.Meta, Size: f.Size, Chunks: len(f.Chunks), Created: f.Created})
}

func (c *coordinator) placeChunk(fileID string, idx int, data []byte) (Chunk, error) {
	sum := sha256.Sum256(data)
	ch := Chunk{ID: fmt.Sprintf("%s-%d", fileID, idx), Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	size := int64(len(data))

	c.mu.Lock()
	targets := c.pickNodesLocked(c.repl, size, nil)
	c.mu.Unlock()
	if len(targets) == 0 {
		return ch, errors.New("no online node with enough free space")
	}
	for _, t := range targets {
		if err := c.putChunk(t, ch.ID, data); err != nil {
			log.Printf("put %s on %s: %v", ch.ID, t.ID, err)
			c.mu.Lock()
			c.releaseLocked(t.ID, size)
			c.mu.Unlock()
			continue
		}
		ch.Nodes = append(ch.Nodes, t.ID)
	}
	if len(ch.Nodes) == 0 {
		return ch, errors.New("could not store chunk on any node")
	}
	return ch, nil // under-replicated chunks are fixed by repairLoop
}

func (c *coordinator) dropChunks(chunks []Chunk) {
	for _, ch := range chunks {
		c.deleteReplicas(ch)
	}
}

func (c *coordinator) deleteReplicas(ch Chunk) {
	for _, id := range ch.Nodes {
		c.mu.Lock()
		ref, ok := c.refLocked(id)
		c.releaseLocked(id, ch.Size)
		c.mu.Unlock()
		if ok {
			if err := c.delChunk(ref, ch.ID); err != nil {
				log.Printf("delete %s on %s: %v", ch.ID, id, err) // orphan stays on offline node
			}
		}
	}
}

func (c *coordinator) ownedFile(w http.ResponseWriter, r *http.Request) *File {
	u := c.auth(r)
	if u == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil
	}
	c.mu.Lock()
	f := c.snap.Files[r.PathValue("id")]
	c.mu.Unlock()
	if f == nil || f.Owner != u.ID {
		http.Error(w, "not found", http.StatusNotFound)
		return nil
	}
	return f
}

func (c *coordinator) download(w http.ResponseWriter, r *http.Request) {
	f := c.ownedFile(w, r)
	if f == nil {
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
	for _, ch := range f.Chunks {
		data, err := c.fetchChunk(ch)
		if err != nil {
			log.Printf("download %s: %v", f.ID, err)
			panic(http.ErrAbortHandler) // headers already sent; abort so the client sees a failure
		}
		w.Write(data)
	}
}

// fetchChunk tries online replicas first and verifies the checksum.
func (c *coordinator) fetchChunk(ch Chunk) ([]byte, error) {
	c.mu.Lock()
	var refs []nodeRef
	for _, id := range ch.Nodes {
		if nd := c.snap.Nodes[id]; nd != nil && c.onlineLocked(nd) {
			refs = append(refs, nodeRef{nd.ID})
		}
	}
	c.mu.Unlock()
	for _, ref := range refs {
		data, err := c.getChunk(ref, ch.ID)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) == ch.SHA256 {
			return data, nil
		}
		log.Printf("chunk %s corrupted on %s", ch.ID, ref.ID)
	}
	return nil, fmt.Errorf("chunk %s unavailable (no healthy online replica)", ch.ID)
}

func (c *coordinator) deleteFile(w http.ResponseWriter, r *http.Request) {
	f := c.ownedFile(w, r)
	if f == nil {
		return
	}
	c.mu.Lock()
	delete(c.snap.Files, f.ID)
	c.saveLocked()
	c.mu.Unlock()
	c.dropChunks(f.Chunks)
	w.WriteHeader(http.StatusNoContent)
}
