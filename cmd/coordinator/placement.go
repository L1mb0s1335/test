package main

import (
	"math/rand"
	"sort"
)

// pickNodesLocked reserves space for size bytes on up to n distinct online
// nodes, preferring the ones with the most free space (so the community fills
// evenly). Nodes in exclude are skipped. Caller must hold c.mu.
func (c *coordinator) pickNodesLocked(n int, size int64, exclude map[string]bool) []nodeRef {
	var cand []*Node
	for _, nd := range c.snap.Nodes {
		if exclude[nd.ID] || !c.onlineLocked(nd) || nd.Capacity-nd.Used < size {
			continue
		}
		cand = append(cand, nd)
	}
	rand.Shuffle(len(cand), func(i, j int) { cand[i], cand[j] = cand[j], cand[i] })
	sort.SliceStable(cand, func(i, j int) bool {
		return cand[i].Capacity-cand[i].Used > cand[j].Capacity-cand[j].Used
	})
	if len(cand) > n {
		cand = cand[:n]
	}
	out := make([]nodeRef, len(cand))
	for i, nd := range cand {
		nd.Used += size
		out[i] = nodeRef{nd.ID, nd.Addr, nd.Key}
	}
	return out
}

func (c *coordinator) releaseLocked(nodeID string, size int64) {
	if nd := c.snap.Nodes[nodeID]; nd != nil {
		nd.Used -= size
		if nd.Used < 0 {
			nd.Used = 0
		}
	}
}

func (c *coordinator) refLocked(id string) (nodeRef, bool) {
	nd := c.snap.Nodes[id]
	if nd == nil {
		return nodeRef{}, false
	}
	return nodeRef{nd.ID, nd.Addr, nd.Key}, true
}
