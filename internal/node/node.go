// Package node is the donor agent: it stores encrypted chunks for the
// community. It only makes outbound HTTP(S) requests, so it works behind NAT.
package node

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"p2pdrive/internal/proto"
)

const workers = 4

var chunkIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type Config struct {
	Server   string
	Token    string // user token
	Dir      string
	Capacity int64
}

// Node is a running donor agent.
type Node struct {
	cfg    Config
	chunks string
	used   atomic.Int64
	id     string
	key    string
	http   *http.Client

	mu      sync.Mutex
	lastErr string
	online  bool
}

type Status struct {
	NodeID   string `json:"node_id"`
	Online   bool   `json:"online"`
	Used     int64  `json:"used"`
	Capacity int64  `json:"capacity"`
	Error    string `json:"error,omitempty"`
}

type identity struct {
	NodeID  string `json:"node_id"`
	NodeKey string `json:"node_key"`
}

func New(cfg Config) *Node {
	return &Node{cfg: cfg, chunks: filepath.Join(cfg.Dir, "chunks"), http: &http.Client{Timeout: 60 * time.Second}}
}

func (n *Node) Status() Status {
	n.mu.Lock()
	defer n.mu.Unlock()
	return Status{NodeID: n.id, Online: n.online, Used: n.used.Load(), Capacity: n.cfg.Capacity, Error: n.lastErr}
}

func (n *Node) setState(online bool, err error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.online = online
	if err != nil {
		n.lastErr = err.Error()
	} else {
		n.lastErr = ""
	}
}

// Run registers with the coordinator and serves jobs until ctx is cancelled.
func (n *Node) Run(ctx context.Context) error {
	if err := os.MkdirAll(n.chunks, 0o700); err != nil {
		return err
	}
	n.scanUsed()
	if err := n.register(ctx); err != nil {
		n.setState(false, err)
		return err
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n.pollLoop(ctx)
		}()
	}
	wg.Wait()
	n.setState(false, nil)
	return nil
}

func (n *Node) scanUsed() {
	var total int64
	entries, _ := os.ReadDir(n.chunks)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && !e.IsDir() {
			total += info.Size()
		}
	}
	n.used.Store(total)
}

func (n *Node) register(ctx context.Context) error {
	idFile := filepath.Join(n.cfg.Dir, "node.json")
	var id identity
	if b, err := os.ReadFile(idFile); err == nil {
		_ = json.Unmarshal(b, &id)
	}
	req := proto.RegisterNodeReq{NodeID: id.NodeID, NodeKey: id.NodeKey, Capacity: n.cfg.Capacity}
	b, _ := json.Marshal(req)
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, n.cfg.Server+"/api/nodes/register", bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer "+n.cfg.Token)
	resp, err := n.http.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("register: %s %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var out proto.RegisterNodeResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	n.id, n.key = out.NodeID, out.NodeKey
	b, _ = json.Marshal(identity{out.NodeID, out.NodeKey})
	return os.WriteFile(idFile, b, 0o600)
}

func (n *Node) pollLoop(ctx context.Context) {
	for ctx.Err() == nil {
		err := n.pollOnce(ctx)
		if err != nil && ctx.Err() == nil {
			n.setState(false, err)
			select {
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
			}
		} else {
			n.setState(true, nil)
		}
	}
}

func (n *Node) url(path string, q ...string) string {
	u := n.cfg.Server + path + "?node=" + n.id
	for i := 0; i+1 < len(q); i += 2 {
		u += "&" + q[i] + "=" + q[i+1]
	}
	return u
}

func (n *Node) pollOnce(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, n.url("/api/nodes/poll"), nil)
	req.Header.Set("Authorization", "Bearer "+n.key)
	resp, err := n.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("poll: %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	jobID := resp.Header.Get(proto.HdrJobID)
	status, out := n.execute(resp.Header.Get(proto.HdrOp), resp.Header.Get(proto.HdrChunkID), body)

	res, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		n.url("/api/nodes/result", "job", jobID, "status", strconv.Itoa(status)), bytes.NewReader(out))
	res.Header.Set("Authorization", "Bearer "+n.key)
	r2, err := n.http.Do(res)
	if err != nil {
		return err
	}
	r2.Body.Close()
	return nil
}

// execute performs a storage job and returns an HTTP-style status.
func (n *Node) execute(op, chunkID string, body []byte) (int, []byte) {
	if !chunkIDRe.MatchString(chunkID) {
		return http.StatusBadRequest, nil
	}
	path := filepath.Join(n.chunks, chunkID)
	switch op {
	case http.MethodPut:
		var old int64
		if st, err := os.Stat(path); err == nil {
			old = st.Size()
		}
		if n.used.Load()-old+int64(len(body)) > n.cfg.Capacity {
			return http.StatusInsufficientStorage, nil
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, body, 0o600); err != nil {
			return http.StatusInternalServerError, nil
		}
		if err := os.Rename(tmp, path); err != nil {
			os.Remove(tmp)
			return http.StatusInternalServerError, nil
		}
		n.used.Add(int64(len(body)) - old)
		return http.StatusNoContent, nil
	case http.MethodGet:
		b, err := os.ReadFile(path)
		if err != nil {
			return http.StatusNotFound, nil
		}
		return http.StatusOK, b
	case http.MethodDelete:
		if st, err := os.Stat(path); err == nil {
			if os.Remove(path) == nil {
				n.used.Add(-st.Size())
			}
		}
		return http.StatusNoContent, nil
	}
	return http.StatusBadRequest, nil
}
