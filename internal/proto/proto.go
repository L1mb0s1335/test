// Package proto holds the wire types shared by coordinator, node and CLI.
package proto

import "time"

const (
	DefaultChunkSize   = 4 << 20 // 4 MiB
	DefaultReplication = 2
)

type RegisterUserReq struct {
	Name string `json:"name"`
}

type RegisterUserResp struct {
	UserID string `json:"user_id"`
	Token  string `json:"token"`
}

type RegisterNodeReq struct {
	NodeID   string `json:"node_id,omitempty"`  // set on restart to keep identity
	NodeKey  string `json:"node_key,omitempty"` // secret returned on first registration
	Addr     string `json:"addr"`               // http://host:port reachable by the coordinator
	Capacity int64  `json:"capacity"`           // bytes donated to the community
}

type RegisterNodeResp struct {
	NodeID  string `json:"node_id"`
	NodeKey string `json:"node_key"`
}

type HeartbeatReq struct {
	NodeID  string `json:"node_id"`
	NodeKey string `json:"node_key"`
}

type NodeInfo struct {
	ID       string    `json:"id"`
	Owner    string    `json:"owner"`
	Addr     string    `json:"addr"`
	Capacity int64     `json:"capacity"`
	Used     int64     `json:"used"`
	Online   bool      `json:"online"`
	LastSeen time.Time `json:"last_seen"`
}

type FileInfo struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Chunks  int       `json:"chunks"`
	Created time.Time `json:"created"`
}

type Stats struct {
	Users        int   `json:"users"`
	NodesTotal   int   `json:"nodes_total"`
	NodesOnline  int   `json:"nodes_online"`
	Capacity     int64 `json:"capacity_online"`
	Used         int64 `json:"used_online"`
	Files        int   `json:"files"`
	LogicalBytes int64 `json:"logical_bytes"`
	Replication  int   `json:"replication"`
}
