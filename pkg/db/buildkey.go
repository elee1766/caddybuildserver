package db

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// BuildConfig represents all the configurable parameters that determine a unique build
type BuildConfig struct {
	CaddyVersion string   `json:"caddy_version"`
	Plugins      []string `json:"plugins"`
	Replacements []string `json:"replacements"`
	OS           string   `json:"os"`
	Arch         string   `json:"arch"`
}

// Normalize returns a new BuildConfig with sorted plugins and replacements for deterministic hashing
func (bc *BuildConfig) Normalize() *BuildConfig {
	// Sort plugins and replacements to ensure deterministic hashing
	sortedPlugins := make([]string, len(bc.Plugins))
	copy(sortedPlugins, bc.Plugins)
	sort.Strings(sortedPlugins)

	sortedReplacements := make([]string, len(bc.Replacements))
	copy(sortedReplacements, bc.Replacements)
	sort.Strings(sortedReplacements)

	return &BuildConfig{
		CaddyVersion: bc.CaddyVersion,
		Plugins:      sortedPlugins,
		Replacements: sortedReplacements,
		OS:           bc.OS,
		Arch:         bc.Arch,
	}
}

// Hash returns the SHA256 hash of the normalized build config as a hex string
func (bc *BuildConfig) Hash() (string, error) {
	normalized := bc.Normalize()
	jsonData, err := json.Marshal(normalized)
	if err != nil {
		return "", fmt.Errorf("failed to marshal build config: %w", err)
	}
	hash := sha256.Sum256(jsonData)
	return hex.EncodeToString(hash[:]), nil
}

// String returns the hash representation of the build config
func (bc *BuildConfig) String() string {
	hash, err := bc.Hash()
	if err != nil {
		return fmt.Sprintf("BuildConfig{error: %v}", err)
	}
	return hash
}
