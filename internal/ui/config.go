package ui

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type NodeConfig struct {
	Enabled    bool   `json:"enabled"`
	Dir        string `json:"dir"`
	CapacityMB int64  `json:"capacity_mb"`
}

type Config struct {
	Server string     `json:"server"`
	Token  string     `json:"token"`
	Node   NodeConfig `json:"node"`
}

// ConfigDir is ~/.config/p2pdrive on Linux, %AppData%\p2pdrive on Windows.
func ConfigDir() string {
	d, err := os.UserConfigDir()
	if err != nil {
		d = "."
	}
	return filepath.Join(d, "p2pdrive")
}

func configPath() string { return filepath.Join(ConfigDir(), "config.json") }

func LoadConfig() Config {
	var c Config
	if b, err := os.ReadFile(configPath()); err == nil {
		_ = json.Unmarshal(b, &c)
	}
	if c.Node.Dir == "" {
		c.Node.Dir = filepath.Join(ConfigDir(), "donated")
	}
	if c.Node.CapacityMB == 0 {
		c.Node.CapacityMB = 10240
	}
	return c
}

func (c Config) Save() error {
	if err := os.MkdirAll(ConfigDir(), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", " ")
	return os.WriteFile(configPath(), b, 0o600)
}
