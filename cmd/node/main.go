// Command node donates local disk space to the community: it stores chunks
// on behalf of the coordinator and keeps a heartbeat going.
package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"p2pdrive/internal/proto"
)

var chunkIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

type identity struct {
	NodeID  string `json:"node_id"`
	NodeKey string `json:"node_key"`
}

type server struct {
	dir string
	key string
}

func main() {
	coord := flag.String("coordinator", "http://localhost:8080", "coordinator URL")
	token := flag.String("token", os.Getenv("DRIVE_TOKEN"), "user token (from `drive register`)")
	listen := flag.String("listen", ":9000", "listen address")
	public := flag.String("public", "", "URL the coordinator uses to reach this node (default http://localhost<listen>)")
	dir := flag.String("dir", "./node-data", "storage directory")
	capMB := flag.Int64("capacity-mb", 1024, "how much space to donate, in MiB")
	flag.Parse()

	if *token == "" {
		log.Fatal("need -token or DRIVE_TOKEN")
	}
	if *public == "" {
		*public = "http://localhost" + *listen
	}
	if err := os.MkdirAll(filepath.Join(*dir, "chunks"), 0o755); err != nil {
		log.Fatal(err)
	}

	id := register(*coord, *token, *public, *dir, *capMB<<20)
	s := &server{dir: filepath.Join(*dir, "chunks"), key: id.NodeKey}

	go func() {
		t := time.NewTicker(5 * time.Second)
		for range t.C {
			hb := proto.HeartbeatReq{NodeID: id.NodeID, NodeKey: id.NodeKey}
			if err := postJSON(*coord+"/api/nodes/heartbeat", "", hb, nil); err != nil {
				log.Printf("heartbeat: %v", err)
			}
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/chunks/", s.chunk)
	log.Printf("node %s serving %d MiB on %s (public %s)", id.NodeID, *capMB, *listen, *public)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

func register(coord, token, public, dir string, capacity int64) identity {
	idFile := filepath.Join(dir, "node.json")
	var id identity
	if b, err := os.ReadFile(idFile); err == nil {
		_ = json.Unmarshal(b, &id)
	}
	req := proto.RegisterNodeReq{NodeID: id.NodeID, NodeKey: id.NodeKey, Addr: public, Capacity: capacity}
	var resp proto.RegisterNodeResp
	if err := postJSON(coord+"/api/nodes/register", token, req, &resp); err != nil {
		log.Fatalf("register: %v", err)
	}
	id = identity{NodeID: resp.NodeID, NodeKey: resp.NodeKey}
	b, _ := json.Marshal(id)
	if err := os.WriteFile(idFile, b, 0o600); err != nil {
		log.Fatal(err)
	}
	return id
}

func (s *server) authed(r *http.Request) bool {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.key)) == 1
}

func (s *server) chunk(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/chunks/")
	if !chunkIDRe.MatchString(id) {
		http.Error(w, "bad chunk id", http.StatusBadRequest)
		return
	}
	path := filepath.Join(s.dir, id)
	switch r.Method {
	case http.MethodPut:
		tmp := path + ".tmp"
		f, err := os.Create(tmp)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_, err = io.Copy(f, r.Body)
		f.Close()
		if err != nil {
			os.Remove(tmp)
			http.Error(w, err.Error(), 500)
			return
		}
		if err := os.Rename(tmp, path); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		http.ServeFile(w, r, path)
	case http.MethodDelete:
		os.Remove(path)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func postJSON(url, token string, in, out any) error {
	b, _ := json.Marshal(in)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
