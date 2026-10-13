package main

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Nodes never accept inbound connections (they sit behind NAT/firewalls).
// Instead each node long-polls the coordinator for jobs and posts results.

type job struct {
	id      string
	node    string
	op      string
	chunkID string
	body    []byte
	done    chan jobResult
}

type jobResult struct {
	status int
	body   []byte
}

type relay struct {
	mu     sync.Mutex
	queues map[string]chan *job // by node id
	jobs   map[string]*job      // in flight, by job id
}

func newRelay() *relay {
	return &relay{queues: map[string]chan *job{}, jobs: map[string]*job{}}
}

func (r *relay) queue(nodeID string) chan *job {
	r.mu.Lock()
	defer r.mu.Unlock()
	q := r.queues[nodeID]
	if q == nil {
		q = make(chan *job, 32)
		r.queues[nodeID] = q
	}
	return q
}

// nodeRef identifies a node; kept as a struct so call sites stay explicit.
type nodeRef struct{ ID string }

func (c *coordinator) nodeCall(n nodeRef, op, chunkID string, body []byte) ([]byte, error) {
	c.mu.Lock()
	nd := c.snap.Nodes[n.ID]
	alive := nd != nil && c.onlineLocked(nd)
	c.mu.Unlock()
	if !alive {
		return nil, fmt.Errorf("node %s: offline", n.ID)
	}
	j := &job{id: randID(8), node: n.ID, op: op, chunkID: chunkID, body: body, done: make(chan jobResult, 1)}
	c.relay.mu.Lock()
	c.relay.jobs[j.id] = j
	c.relay.mu.Unlock()
	defer func() {
		c.relay.mu.Lock()
		delete(c.relay.jobs, j.id)
		c.relay.mu.Unlock()
	}()

	select {
	case c.relay.queue(n.ID) <- j:
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("node %s: queue full", n.ID)
	}
	select {
	case res := <-j.done:
		if res.status >= 300 {
			return nil, fmt.Errorf("node %s: status %d", n.ID, res.status)
		}
		return res.body, nil
	case <-time.After(30 * time.Second):
		return nil, errors.New("node " + n.ID + ": timeout")
	}
}

func (c *coordinator) putChunk(n nodeRef, id string, data []byte) error {
	_, err := c.nodeCall(n, http.MethodPut, id, data)
	return err
}

func (c *coordinator) getChunk(n nodeRef, id string) ([]byte, error) {
	return c.nodeCall(n, http.MethodGet, id, nil)
}

func (c *coordinator) delChunk(n nodeRef, id string) error {
	_, err := c.nodeCall(n, http.MethodDelete, id, nil)
	return err
}
