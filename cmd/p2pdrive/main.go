// Command p2pdrive is the desktop app (Windows/Linux) for the community drive.
//
//	p2pdrive                 open the app (local UI in your browser)
//	p2pdrive node            donate space without the UI (headless)
//	p2pdrive register NAME   create an account
//	p2pdrive ls|put|get|rm   command-line file access (needs DRIVE_PASSPHRASE)
//
// All encryption happens locally; see README for the security model.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"p2pdrive/internal/client"
	"p2pdrive/internal/node"
	"p2pdrive/internal/ui"
	"p2pdrive/internal/vault"
)

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(1)
}

func mb(b int64) string { return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20)) }

// cli builds an unlocked client from saved config + DRIVE_PASSPHRASE.
func cli() *client.Client {
	cfg := ui.LoadConfig()
	if cfg.Server == "" {
		die("not configured: run `p2pdrive` and connect first")
	}
	c := client.New(cfg.Server, cfg.Token)
	pass := os.Getenv("DRIVE_PASSPHRASE")
	if pass == "" {
		die("set DRIVE_PASSPHRASE")
	}
	if err := c.Unlock(pass, false); err != nil {
		die("unlock: %v", err)
	}
	return c
}

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		if err := ui.Run(true); err != nil {
			die("%v", err)
		}
		return
	}
	switch args[0] {
	case "ui":
		if err := ui.Run(false); err != nil {
			die("%v", err)
		}
	case "register":
		if len(args) != 3 {
			die("usage: p2pdrive register <coordinator-url> <name>")
		}
		c := client.New(args[1], "")
		if err := c.Register(args[2]); err != nil {
			die("%v", err)
		}
		cfg := ui.LoadConfig()
		cfg.Server, cfg.Token = c.Server, c.Token
		if err := cfg.Save(); err != nil {
			die("%v", err)
		}
		fmt.Println("account created; token saved. Your token (keep it for other devices):", c.Token)
		if pass := os.Getenv("DRIVE_PASSPHRASE"); pass != "" {
			if err := c.Unlock(pass, true); err != nil {
				die("set passphrase: %v", err)
			}
			fmt.Println("passphrase set")
		} else {
			fmt.Println("now set DRIVE_PASSPHRASE and run `p2pdrive unlock-init`, or use the UI")
		}
	case "unlock-init":
		cfg := ui.LoadConfig()
		c := client.New(cfg.Server, cfg.Token)
		if err := c.Unlock(os.Getenv("DRIVE_PASSPHRASE"), true); err != nil {
			die("%v", err)
		}
		fmt.Println("ok")
	case "node":
		cfg := ui.LoadConfig()
		if cfg.Server == "" {
			die("not configured: run `p2pdrive register` or the UI first")
		}
		n := node.New(node.Config{Server: cfg.Server, Token: cfg.Token, Dir: cfg.Node.Dir, Capacity: cfg.Node.CapacityMB << 20})
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		fmt.Printf("donating %d MiB in %s (Ctrl+C to stop)\n", cfg.Node.CapacityMB, cfg.Node.Dir)
		if err := n.Run(ctx); err != nil {
			die("%v", err)
		}
	case "ls":
		files, err := cli().List()
		if err != nil {
			die("%v", err)
		}
		for _, f := range files {
			fmt.Printf("%s  %10s  %s\n", f.ID, mb(f.Size), f.Name)
		}
	case "put":
		if len(args) != 2 {
			die("usage: p2pdrive put <file>")
		}
		f, err := os.Open(args[1])
		if err != nil {
			die("%v", err)
		}
		defer f.Close()
		st, _ := f.Stat()
		up, err := cli().Upload(filepath.Base(args[1]), st.Size(), f)
		if err != nil {
			die("%v", err)
		}
		fmt.Printf("%s  %s  %s\n", up.ID, up.Name, mb(up.Size))
	case "get":
		if len(args) != 3 {
			die("usage: p2pdrive get <id> <out>")
		}
		info, rc, err := cli().Open(args[1])
		if err != nil {
			die("%v", err)
		}
		defer rc.Close()
		tmp := args[2] + ".part"
		out, err := os.Create(tmp)
		if err != nil {
			die("%v", err)
		}
		_, err = io.Copy(out, rc)
		out.Close()
		if err != nil {
			os.Remove(tmp)
			if err == vault.ErrCorrupt {
				die("INTEGRITY CHECK FAILED: %v", err)
			}
			die("%v", err)
		}
		if err := os.Rename(tmp, args[2]); err != nil {
			die("%v", err)
		}
		fmt.Printf("saved %s (%s)\n", args[2], mb(info.Size))
	case "rm":
		if len(args) != 2 {
			die("usage: p2pdrive rm <id>")
		}
		if err := cli().Delete(args[1]); err != nil {
			die("%v", err)
		}
	default:
		die("unknown command %q\n%s", args[0], strings.TrimSpace(`commands: ui, node, register, unlock-init, ls, put, get, rm`))
	}
}
