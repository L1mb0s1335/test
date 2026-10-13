package main

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
	"time"
)

// repairLoop re-replicates chunks whose number of online replicas fell below
// the target, e.g. because a donor switched their computer off.
func (c *coordinator) repairLoop(every time.Duration) {
	for range time.Tick(every) {
		c.repairOnce()
	}
}

type repairJob struct {
	fileID  string
	idx     int
	chunk   Chunk
	sources []nodeRef
}

func (c *coordinator) repairOnce() {
	c.mu.Lock()
	var jobs []repairJob
	for _, f := range c.snap.Files {
		for i, ch := range f.Chunks {
			var src []nodeRef
			for _, id := range ch.Nodes {
				if nd := c.snap.Nodes[id]; nd != nil && c.onlineLocked(nd) {
					src = append(src, nodeRef{nd.ID})
				}
			}
			if len(src) > 0 && len(src) < c.repl {
				jobs = append(jobs, repairJob{f.ID, i, ch, src})
			}
		}
	}
	c.mu.Unlock()

	for _, j := range jobs {
		c.repairChunk(j)
	}
}

func (c *coordinator) repairChunk(j repairJob) {
	var data []byte
	for _, s := range j.sources {
		b, err := c.getChunk(s, j.chunk.ID)
		if err != nil {
			continue
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) == j.chunk.SHA256 {
			data = b
			break
		}
	}
	if data == nil {
		log.Printf("repair %s: no healthy source", j.chunk.ID)
		return
	}

	c.mu.Lock()
	f := c.snap.Files[j.fileID]
	if f == nil { // deleted meanwhile
		c.mu.Unlock()
		return
	}
	have := map[string]bool{}
	for _, id := range j.chunk.Nodes {
		have[id] = true
	}
	need := c.repl - len(j.sources)
	targets := c.pickNodesLocked(need, j.chunk.Size, have)
	c.mu.Unlock()

	for _, t := range targets {
		err := c.putChunk(t, j.chunk.ID, data)
		c.mu.Lock()
		if err != nil || c.snap.Files[j.fileID] == nil {
			c.releaseLocked(t.ID, j.chunk.Size)
		} else {
			ch := &f.Chunks[j.idx]
			ch.Nodes = append(ch.Nodes, t.ID)
			log.Printf("repair %s: replicated to %s", ch.ID, t.ID)
			c.saveLocked()
		}
		c.mu.Unlock()
		if err != nil {
			log.Printf("repair %s -> %s: %v", j.chunk.ID, t.ID, err)
		}
	}
}
