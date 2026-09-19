package main

import (
	"encoding/json"
	"log"
	"os"
	"strings"
	"sync"
)

const (
	defaultWindowTitle      = "Lingma Proxy"
	defaultSingleInstanceID = "lingma-proxy-desktop"
)

// instanceProfile lets a second copy of the desktop app run beside the first one.
// Without it Wails would fold the second launch into the first window (the
// single-instance lock is keyed by UniqueId), and both copies would rewrite the
// same shared settings and dashboard state files.
//
// A copy opts in by carrying a lingma-proxy.json next to its executable with
// "instance_name" and "instance_id" set.
type instanceProfile struct {
	title    string
	singleID string
	// configPath is the file the settings were loaded from. Saving writes back
	// there, so a per-installation sidecar is not replaced by the shared
	// ~/.config copy that the other instance uses.
	configPath string
}

var (
	instanceMu     sync.Mutex
	instanceCached *instanceProfile
)

// LoadInstanceProfile reads the profile once, before the window title and the
// single-instance id are needed.
func LoadInstanceProfile() instanceProfile {
	instanceMu.Lock()
	defer instanceMu.Unlock()
	if instanceCached == nil {
		instanceCached = readInstanceProfile()
	}
	return *instanceCached
}

func instanceProfileValue() instanceProfile { return LoadInstanceProfile() }

func readInstanceProfile() *instanceProfile {
	profile := &instanceProfile{title: defaultWindowTitle, singleID: defaultSingleInstanceID}
	for _, path := range configSearchPaths() {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var doc struct {
			InstanceName string `json:"instance_name"`
			InstanceID   string `json:"instance_id"`
		}
		if err := json.Unmarshal(data, &doc); err != nil {
			// Falling through to the next file would silently inherit the other
			// instance's single-id and settings, so say so.
			log.Printf("desktop: %s is not readable as JSON (%v); it will be ignored", path, err)
			continue
		}
		profile.configPath = path
		if name := strings.TrimSpace(doc.InstanceName); name != "" {
			profile.title = name
		}
		if id := strings.TrimSpace(doc.InstanceID); id != "" {
			profile.singleID = id
		}
		return profile
	}
	return profile
}
