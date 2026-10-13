package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Token string `json:"token"`
}

type Node struct {
	ID       string    `json:"id"`
	Owner    string    `json:"owner"`
	Key      string    `json:"key"`
	Addr     string    `json:"addr"`
	Capacity int64     `json:"capacity"`
	Used     int64     `json:"used"` // bytes the coordinator has placed here
	LastSeen time.Time `json:"last_seen"`
}

type Chunk struct {
	ID     string   `json:"id"`
	Size   int64    `json:"size"`
	SHA256 string   `json:"sha256"`
	Nodes  []string `json:"nodes"`
}

type File struct {
	ID      string    `json:"id"`
	Owner   string    `json:"owner"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Chunks  []Chunk   `json:"chunks"`
	Created time.Time `json:"created"`
}

type snapshot struct {
	Users map[string]*User `json:"users"` // by token
	Nodes map[string]*Node `json:"nodes"` // by id
	Files map[string]*File `json:"files"` // by id
}

type coordinator struct {
	mu        sync.Mutex
	path      string
	repl      int
	chunkSize int
	offline   time.Duration
	snap      snapshot
}

func newCoordinator(path string, repl, chunkSize int, offline time.Duration) *coordinator {
	c := &coordinator{path: path, repl: repl, chunkSize: chunkSize, offline: offline}
	c.snap = snapshot{Users: map[string]*User{}, Nodes: map[string]*Node{}, Files: map[string]*File{}}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &c.snap); err != nil {
			log.Fatalf("bad state file: %v", err)
		}
	}
	return c
}

// saveLocked persists metadata atomically. Caller must hold c.mu.
func (c *coordinator) saveLocked() {
	b, err := json.MarshalIndent(&c.snap, "", " ")
	if err != nil {
		log.Printf("save: %v", err)
		return
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("save: %v", err)
		return
	}
	if err := os.Rename(tmp, c.path); err != nil {
		log.Printf("save: %v", err)
	}
}

func (c *coordinator) onlineLocked(n *Node) bool {
	return time.Since(n.LastSeen) < c.offline
}

func randID(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
