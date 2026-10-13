package main

import (
	"crypto/sha256"
	"crypto/subtle"
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
	mux.HandleFunc("POST /api/nodes/register", c.registerNode)
	mux.HandleFunc("POST /api/nodes/heartbeat", c.heartbeat)
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

func (c *coordinator) registerNode(w http.ResponseWriter, r *http.Request) {
	u := c.auth(r)
	if u == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req proto.RegisterNodeReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || req.Addr == "" || req.Capacity <= 0 {
		http.Error(w, "addr and positive capacity required", http.StatusBadRequest)
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
	n.Addr, n.Capacity, n.LastSeen = strings.TrimRight(req.Addr, "/"), req.Capacity, time.Now()
	c.saveLocked()
	log.Printf("node %s registered: %s, %d MiB", n.ID, n.Addr, n.Capacity>>20)
	writeJSON(w, proto.RegisterNodeResp{NodeID: n.ID, NodeKey: n.Key})
}

func (c *coordinator) heartbeat(w http.ResponseWriter, r *http.Request) {
	var req proto.HeartbeatReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.snap.Nodes[req.NodeID]
	if n == nil || subtle.ConstantTimeCompare([]byte(n.Key), []byte(req.NodeKey)) != 1 {
		http.Error(w, "unknown node", http.StatusForbidden)
		return
	}
	n.LastSeen = time.Now() // not persisted on every beat; saved on next change
	w.WriteHeader(http.StatusNoContent)
}

func (c *coordinator) listNodes(w http.ResponseWriter, r *http.Request) {
	if c.auth(r) == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c.mu.Lock()
	out := []proto.NodeInfo{}
	for _, n := range c.snap.Nodes {
		out = append(out, proto.NodeInfo{ID: n.ID, Owner: n.Owner, Addr: n.Addr, Capacity: n.Capacity,
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
	for _, f := range c.snap.Files {
		s.LogicalBytes += f.Size
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
			out = append(out, proto.FileInfo{ID: f.ID, Name: f.Name, Size: f.Size, Chunks: len(f.Chunks), Created: f.Created})
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
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "?name= required", http.StatusBadRequest)
		return
	}
	f := &File{ID: randID(8), Owner: u.ID, Name: name, Created: time.Now()}
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
	writeJSON(w, proto.FileInfo{ID: f.ID, Name: f.Name, Size: f.Size, Chunks: len(f.Chunks), Created: f.Created})
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
		if err := putChunk(t, ch.ID, data); err != nil {
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
			if err := delChunk(ref, ch.ID); err != nil {
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
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", f.Name))
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
			refs = append(refs, nodeRef{nd.ID, nd.Addr, nd.Key})
		}
	}
	c.mu.Unlock()
	for _, ref := range refs {
		data, err := getChunk(ref, ch.ID)
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
