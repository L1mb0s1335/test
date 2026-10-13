// Package client talks to the coordinator and performs all encryption, so
// plaintext and keys never leave the user's machine.
package client

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"p2pdrive/internal/proto"
	"p2pdrive/internal/vault"
)

var (
	ErrLocked = errors.New("vault is locked: enter your passphrase")
	ErrNoKey  = errors.New("no passphrase set yet for this account")
)

type Client struct {
	Server string
	Token  string
	http   *http.Client

	mu sync.Mutex
	v  *vault.Vault // nil while locked
}

func New(server, token string) *Client {
	return &Client{
		Server: strings.TrimRight(server, "/"),
		Token:  token,
		http: &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		}},
	}
}

func (c *Client) do(method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, c.Server+path, body)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("server: %s %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

func (c *Client) getJSON(path string, out any) error {
	resp, err := c.do(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

// Register creates a new account and stores the token on the client.
func (c *Client) Register(name string) error {
	b, _ := json.Marshal(proto.RegisterUserReq{Name: name})
	resp, err := c.do(http.MethodPost, "/api/users", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var r proto.RegisterUserResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return err
	}
	c.Token = r.Token
	return nil
}

func (c *Client) Me() (*proto.Me, error) {
	var m proto.Me
	return &m, c.getJSON("/api/me", &m)
}

// Unlocked reports whether the passphrase has been entered this session.
func (c *Client) Unlocked() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.v != nil
}

func (c *Client) Lock() {
	c.mu.Lock()
	c.v = nil
	c.mu.Unlock()
}

// Unlock verifies the passphrase against the server-stored check. If the
// account has no key yet, it initialises it (first use) — only when create is true.
func (c *Client) Unlock(passphrase string, create bool) error {
	me, err := c.Me()
	if err != nil {
		return err
	}
	var v *vault.Vault
	if me.Salt == nil {
		if !create {
			return ErrNoKey
		}
		if len(passphrase) < 8 {
			return errors.New("passphrase must be at least 8 characters")
		}
		salt := vault.NewSalt()
		v = vault.Unlock(passphrase, salt)
		b, _ := json.Marshal(proto.KeyInfo{Salt: salt, Check: v.MakeCheck()})
		resp, err := c.do(http.MethodPut, "/api/keyinfo", bytes.NewReader(b))
		if err != nil {
			return err
		}
		resp.Body.Close()
	} else {
		v = vault.Unlock(passphrase, me.Salt)
		if err := v.VerifyCheck(me.Check); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.v = v
	c.mu.Unlock()
	return nil
}

func (c *Client) vault() (*vault.Vault, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.v == nil {
		return nil, ErrLocked
	}
	return c.v, nil
}

// File is a decrypted directory entry.
type File struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"` // plaintext size
	Created time.Time `json:"created"`
}

func (c *Client) List() ([]File, error) {
	v, err := c.vault()
	if err != nil {
		return nil, err
	}
	var infos []proto.FileInfo
	if err := c.getJSON("/api/files", &infos); err != nil {
		return nil, err
	}
	out := make([]File, 0, len(infos))
	for _, fi := range infos {
		m, err := v.OpenMeta(fi.Meta)
		if err != nil {
			continue // not decryptable with this key; skip
		}
		out = append(out, File{ID: fi.ID, Name: m.Name, Size: m.Size, Created: fi.Created})
	}
	return out, nil
}

// Upload encrypts r on the fly and streams it to the coordinator. size is the
// plaintext length (informational, stored encrypted).
func (c *Client) Upload(name string, size int64, r io.Reader) (*File, error) {
	v, err := c.vault()
	if err != nil {
		return nil, err
	}
	m := vault.NewFileMeta(name)
	m.Size = size
	meta := base64.RawURLEncoding.EncodeToString(v.SealMeta(m))
	resp, err := c.do(http.MethodPost, "/api/files?meta="+meta, vault.EncryptReader(r, m.Key))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var fi proto.FileInfo
	if err := json.NewDecoder(resp.Body).Decode(&fi); err != nil {
		return nil, err
	}
	return &File{ID: fi.ID, Name: name, Size: size, Created: fi.Created}, nil
}

// Open starts downloading a file and returns its decrypted description and a
// reader of the plaintext. Reading to the end authenticates the whole file:
// tampering or truncation surfaces as vault.ErrCorrupt, so callers writing to
// disk must discard the output on error. Close the reader when done.
func (c *Client) Open(id string) (*File, io.ReadCloser, error) {
	v, err := c.vault()
	if err != nil {
		return nil, nil, err
	}
	var infos []proto.FileInfo
	if err := c.getJSON("/api/files", &infos); err != nil {
		return nil, nil, err
	}
	for _, fi := range infos {
		if fi.ID != id {
			continue
		}
		m, err := v.OpenMeta(fi.Meta)
		if err != nil {
			return nil, nil, err
		}
		resp, err := c.do(http.MethodGet, "/api/files/"+id, nil)
		if err != nil {
			return nil, nil, err
		}
		pr := &sizeCheck{r: vault.DecryptReader(resp.Body, m.Key), want: m.Size, c: resp.Body}
		return &File{ID: id, Name: m.Name, Size: m.Size, Created: fi.Created}, pr, nil
	}
	return nil, nil, errors.New("file not found")
}

// sizeCheck turns a clean EOF at the wrong plaintext length into ErrCorrupt.
type sizeCheck struct {
	r    io.Reader
	c    io.Closer
	n    int64
	want int64
}

func (s *sizeCheck) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.n += int64(n)
	if err == io.EOF && s.n != s.want {
		err = vault.ErrCorrupt
	}
	return n, err
}

func (s *sizeCheck) Close() error { return s.c.Close() }

func (c *Client) Delete(id string) error {
	resp, err := c.do(http.MethodDelete, "/api/files/"+id, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (c *Client) Nodes() ([]proto.NodeInfo, error) {
	var ns []proto.NodeInfo
	return ns, c.getJSON("/api/nodes", &ns)
}

func (c *Client) Stats() (*proto.Stats, error) {
	var s proto.Stats
	return &s, c.getJSON("/api/stats", &s)
}
