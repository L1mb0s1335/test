package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"time"
)

var nodeHTTP = &http.Client{Timeout: 60 * time.Second}

// nodeRef is an immutable copy of what is needed to talk to a node.
type nodeRef struct{ ID, Addr, Key string }

func nodeCall(n nodeRef, method, chunkID string, body []byte) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, n.Addr+"/chunks/"+chunkID, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+n.Key)
	resp, err := nodeHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("node %s: %s", n.ID, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func putChunk(n nodeRef, id string, data []byte) error {
	_, err := nodeCall(n, http.MethodPut, id, data)
	return err
}

func getChunk(n nodeRef, id string) ([]byte, error) { return nodeCall(n, http.MethodGet, id, nil) }

func delChunk(n nodeRef, id string) error {
	_, err := nodeCall(n, http.MethodDelete, id, nil)
	return err
}
