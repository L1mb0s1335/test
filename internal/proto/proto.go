// Package proto holds the wire types shared by coordinator, node and client.
package proto

import "time"

type RegisterUserReq struct {
	Name string `json:"name"`
}

type RegisterUserResp struct {
	UserID string `json:"user_id"`
	Token  string `json:"token"`
}

// Me describes the caller. Salt and Check are public KDF parameters that let
// any device derive the same key from the passphrase; they reveal nothing.
type Me struct {
	UserID string `json:"user_id"`
	Name   string `json:"name"`
	Salt   []byte `json:"salt,omitempty"`
	Check  []byte `json:"check,omitempty"`
}

type KeyInfo struct {
	Salt  []byte `json:"salt"`
	Check []byte `json:"check"`
}

type RegisterNodeReq struct {
	NodeID   string `json:"node_id,omitempty"`  // set on restart to keep identity
	NodeKey  string `json:"node_key,omitempty"` // secret returned on first registration
	Capacity int64  `json:"capacity"`           // bytes donated to the community
}

type RegisterNodeResp struct {
	NodeID  string `json:"node_id"`
	NodeKey string `json:"node_key"`
}

// Job headers used on the node long-poll channel.
const (
	HdrJobID   = "X-Job-Id"
	HdrOp      = "X-Op" // PUT | GET | DELETE
	HdrChunkID = "X-Chunk-Id"
)

type NodeInfo struct {
	ID       string    `json:"id"`
	Mine     bool      `json:"mine"`
	Capacity int64     `json:"capacity"`
	Used     int64     `json:"used"`
	Online   bool      `json:"online"`
	LastSeen time.Time `json:"last_seen"`
}

// FileInfo is what the server knows about a file: an opaque encrypted Meta
// blob and the ciphertext size.
type FileInfo struct {
	ID      string    `json:"id"`
	Meta    []byte    `json:"meta"`
	Size    int64     `json:"size"`
	Chunks  int       `json:"chunks"`
	Created time.Time `json:"created"`
}

type Stats struct {
	Users       int   `json:"users"`
	NodesTotal  int   `json:"nodes_total"`
	NodesOnline int   `json:"nodes_online"`
	Capacity    int64 `json:"capacity_online"`
	Used        int64 `json:"used_online"`
	Files       int   `json:"files"`
	Replication int   `json:"replication"`
}
