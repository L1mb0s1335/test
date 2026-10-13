// Package ui serves the desktop app's interface on 127.0.0.1. All crypto
// happens in this process; the browser only ever talks to localhost.
package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"

	"p2pdrive/internal/client"
	"p2pdrive/internal/node"
	"p2pdrive/internal/vault"
)

//go:embed index.html
var assets embed.FS

type App struct {
	secret string
	host   string

	mu     sync.Mutex
	cfg    Config
	cl     *client.Client
	node   *node.Node
	cancel context.CancelFunc
}

func Run(open bool) error {
	b := make([]byte, 24)
	rand.Read(b)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	a := &App{secret: hex.EncodeToString(b), host: ln.Addr().String(), cfg: LoadConfig()}
	if a.cfg.Server != "" {
		a.cl = client.New(a.cfg.Server, a.cfg.Token)
	}
	if a.cfg.Node.Enabled && a.cl != nil {
		if err := a.startNode(); err != nil {
			log.Printf("node autostart: %v", err)
		}
	}
	link := fmt.Sprintf("http://%s/?s=%s", a.host, a.secret)
	fmt.Println("p2pdrive is running. Open:", link)
	if open {
		openBrowser(link)
	}
	return http.Serve(ln, a.routes())
}

func openBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	case "darwin":
		cmd = exec.Command("open", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	_ = cmd.Start()
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.index)
	mux.HandleFunc("GET /api/state", a.state)
	mux.HandleFunc("POST /api/connect", a.connect)
	mux.HandleFunc("POST /api/unlock", a.unlock)
	mux.HandleFunc("POST /api/lock", a.lock)
	mux.HandleFunc("GET /api/files", a.files)
	mux.HandleFunc("POST /api/upload", a.upload)
	mux.HandleFunc("GET /api/download/{id}", a.download)
	mux.HandleFunc("DELETE /api/files/{id}", a.deleteFile)
	mux.HandleFunc("GET /api/community", a.community)
	mux.HandleFunc("POST /api/node/start", a.nodeStart)
	mux.HandleFunc("POST /api/node/stop", a.nodeStop)
	return a.guard(mux)
}

// guard defends the localhost service against other websites and processes:
// Host check (DNS rebinding), secret cookie, and a custom header on writes
// (cross-site requests cannot set it without a CORS preflight, which we refuse).
func (a *App) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != a.host {
			http.Error(w, "bad host", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/" && r.URL.Query().Get("s") != "" {
			if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("s")), []byte(a.secret)) == 1 {
				http.SetCookie(w, &http.Cookie{Name: "p2pd", Value: a.secret, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
		}
		ck, err := r.Cookie("p2pd")
		if err != nil || subtle.ConstantTimeCompare([]byte(ck.Value), []byte(a.secret)) != 1 {
			http.Error(w, "open the link printed by p2pdrive (it contains the access key)", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("X-P2P") != "1" {
			http.Error(w, "missing header", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) index(w http.ResponseWriter, r *http.Request) {
	b, _ := assets.ReadFile("index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'")
	w.Write(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	writeJSON2(w, code, map[string]string{"error": err.Error()})
}

func writeJSON2(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (a *App) client() (*client.Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cl == nil {
		return nil, errors.New("not connected to a coordinator")
	}
	return a.cl, nil
}

func (a *App) state(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	st := map[string]any{
		"connected": a.cl != nil,
		"server":    a.cfg.Server,
		"unlocked":  a.cl != nil && a.cl.Unlocked(),
		"node_cfg":  a.cfg.Node,
	}
	if a.node != nil {
		st["node"] = a.node.Status()
	}
	cl := a.cl
	a.mu.Unlock()
	if cl != nil {
		if me, err := cl.Me(); err == nil {
			st["user"], st["has_key"] = me.Name, me.Salt != nil
		} else {
			st["error"] = err.Error()
		}
	}
	writeJSON(w, st)
}

func (a *App) connect(w http.ResponseWriter, r *http.Request) {
	var req struct{ Server, Token, Name string }
	if json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&req) != nil {
		fail(w, 400, errors.New("bad request"))
		return
	}
	if u, err := url.Parse(req.Server); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		fail(w, 400, errors.New("server must look like https://host:port"))
		return
	}
	cl := client.New(req.Server, req.Token)
	if req.Name != "" { // create a new account
		if err := cl.Register(req.Name); err != nil {
			fail(w, 502, err)
			return
		}
	} else if _, err := cl.Me(); err != nil {
		fail(w, 502, err)
		return
	}
	a.mu.Lock()
	a.stopNodeLocked()
	a.cl, a.cfg.Server, a.cfg.Token = cl, cl.Server, cl.Token
	err := a.cfg.Save()
	a.mu.Unlock()
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, map[string]string{"token": cl.Token})
}

func (a *App) unlock(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Passphrase string
		Create     bool
	}
	cl, err := a.client()
	if err != nil || json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&req) != nil {
		fail(w, 400, errors.New("not connected"))
		return
	}
	if err := cl.Unlock(req.Passphrase, req.Create); err != nil {
		fail(w, 401, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) lock(w http.ResponseWriter, r *http.Request) {
	if cl, err := a.client(); err == nil {
		cl.Lock()
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) files(w http.ResponseWriter, r *http.Request) {
	cl, err := a.client()
	if err == nil {
		var fs []client.File
		if fs, err = cl.List(); err == nil {
			writeJSON(w, fs)
			return
		}
	}
	fail(w, 400, err)
}

func (a *App) upload(w http.ResponseWriter, r *http.Request) {
	cl, err := a.client()
	if err != nil {
		fail(w, 400, err)
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" || r.ContentLength < 0 {
		fail(w, 400, errors.New("name and Content-Length required"))
		return
	}
	f, err := cl.Upload(name, r.ContentLength, r.Body)
	if err != nil {
		fail(w, 502, err)
		return
	}
	writeJSON(w, f)
}

func (a *App) download(w http.ResponseWriter, r *http.Request) {
	cl, err := a.client()
	if err != nil {
		fail(w, 400, err)
		return
	}
	f, rc, err := cl.Open(r.PathValue("id"))
	if err != nil {
		fail(w, 404, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(f.Size, 10))
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(f.Name))
	if _, err := io.Copy(w, rc); err != nil {
		log.Printf("download %s: %v", f.ID, err)
		if errors.Is(err, vault.ErrCorrupt) {
			log.Printf("download %s: integrity check FAILED", f.ID)
		}
		panic(http.ErrAbortHandler) // make the browser mark the download as failed
	}
}

func (a *App) deleteFile(w http.ResponseWriter, r *http.Request) {
	cl, err := a.client()
	if err == nil {
		if err = cl.Delete(r.PathValue("id")); err == nil {
			writeJSON(w, map[string]bool{"ok": true})
			return
		}
	}
	fail(w, 400, err)
}

func (a *App) community(w http.ResponseWriter, r *http.Request) {
	cl, err := a.client()
	if err != nil {
		fail(w, 400, err)
		return
	}
	st, err1 := cl.Stats()
	ns, err2 := cl.Nodes()
	if err1 != nil || err2 != nil {
		fail(w, 502, errors.Join(err1, err2))
		return
	}
	writeJSON(w, map[string]any{"stats": st, "nodes": ns})
}

// ---- donating storage ----

func (a *App) startNode() error {
	if a.cl == nil {
		return errors.New("not connected")
	}
	if err := os.MkdirAll(a.cfg.Node.Dir, 0o700); err != nil {
		return err
	}
	a.stopNodeLocked()
	n := node.New(node.Config{Server: a.cl.Server, Token: a.cl.Token, Dir: a.cfg.Node.Dir, Capacity: a.cfg.Node.CapacityMB << 20})
	ctx, cancel := context.WithCancel(context.Background())
	a.node, a.cancel = n, cancel
	go func() {
		if err := n.Run(ctx); err != nil {
			log.Printf("node: %v", err)
		}
	}()
	return nil
}

func (a *App) stopNodeLocked() {
	if a.cancel != nil {
		a.cancel()
		a.cancel, a.node = nil, nil
	}
}

func (a *App) nodeStart(w http.ResponseWriter, r *http.Request) {
	var req NodeConfig
	if json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&req) != nil || req.CapacityMB <= 0 || req.Dir == "" {
		fail(w, 400, errors.New("dir and positive capacity_mb required"))
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	req.Enabled = true
	a.cfg.Node = req
	if err := a.startNode(); err != nil {
		fail(w, 500, err)
		return
	}
	_ = a.cfg.Save()
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) nodeStop(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopNodeLocked()
	a.cfg.Node.Enabled = false
	_ = a.cfg.Save()
	writeJSON(w, map[string]bool{"ok": true})
}
