// Command drive is the user-facing CLI.
//
//	drive register <name>
//	drive upload <file>
//	drive ls
//	drive get <id> <out>
//	drive rm <id>
//	drive nodes
//	drive stats
//
// Config via DRIVE_SERVER (default http://localhost:8080) and DRIVE_TOKEN.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"p2pdrive/internal/proto"
)

var (
	server = envOr("DRIVE_SERVER", "http://localhost:8080")
	token  = os.Getenv("DRIVE_TOKEN")
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(1)
}

func do(method, path string, body io.Reader) *http.Response {
	req, err := http.NewRequest(method, server+path, body)
	if err != nil {
		die("%v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		die("%v", err)
	}
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		die("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return resp
}

func getJSON(path string, out any) {
	resp := do("GET", path, nil)
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		die("%v", err)
	}
}

func mb(b int64) string { return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20)) }

func main() {
	if len(os.Args) < 2 {
		die("usage: drive register|upload|ls|get|rm|nodes|stats")
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "register":
		if len(args) != 1 {
			die("usage: drive register <name>")
		}
		b, _ := json.Marshal(proto.RegisterUserReq{Name: args[0]})
		resp := do("POST", "/api/users", bytes.NewReader(b))
		var r proto.RegisterUserResp
		json.NewDecoder(resp.Body).Decode(&r)
		fmt.Printf("export DRIVE_TOKEN=%s\n", r.Token)
	case "upload":
		if len(args) != 1 {
			die("usage: drive upload <file>")
		}
		f, err := os.Open(args[0])
		if err != nil {
			die("%v", err)
		}
		defer f.Close()
		resp := do("POST", "/api/files?name="+url.QueryEscape(filepath.Base(args[0])), f)
		var fi proto.FileInfo
		json.NewDecoder(resp.Body).Decode(&fi)
		fmt.Printf("%s  %s  %s  (%d chunks)\n", fi.ID, fi.Name, mb(fi.Size), fi.Chunks)
	case "ls":
		var files []proto.FileInfo
		getJSON("/api/files", &files)
		for _, f := range files {
			fmt.Printf("%s  %10s  %s\n", f.ID, mb(f.Size), f.Name)
		}
	case "get":
		if len(args) != 2 {
			die("usage: drive get <id> <out>")
		}
		resp := do("GET", "/api/files/"+args[0], nil)
		defer resp.Body.Close()
		out, err := os.Create(args[1])
		if err != nil {
			die("%v", err)
		}
		n, err := io.Copy(out, resp.Body)
		out.Close()
		if err != nil || (resp.ContentLength >= 0 && n != resp.ContentLength) {
			os.Remove(args[1])
			die("download failed after %d bytes: %v", n, err)
		}
		fmt.Printf("saved %s (%s)\n", args[1], mb(n))
	case "rm":
		if len(args) != 1 {
			die("usage: drive rm <id>")
		}
		do("DELETE", "/api/files/"+args[0], nil).Body.Close()
	case "nodes":
		var ns []proto.NodeInfo
		getJSON("/api/nodes", &ns)
		for _, n := range ns {
			st := "offline"
			if n.Online {
				st = "online"
			}
			fmt.Printf("%s  %-7s  %s / %s  %s\n", n.ID, st, mb(n.Used), mb(n.Capacity), n.Addr)
		}
	case "stats":
		var s proto.Stats
		getJSON("/api/stats", &s)
		fmt.Printf("users=%d nodes=%d/%d online  pool=%s used=%s  files=%d (%s logical, x%d replication)\n",
			s.Users, s.NodesOnline, s.NodesTotal, mb(s.Capacity), mb(s.Used), s.Files, mb(s.LogicalBytes), s.Replication)
	default:
		die("unknown command %q", os.Args[1])
	}
}
