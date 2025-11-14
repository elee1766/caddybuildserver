package modversion

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/mod/module"
)

// Resolver resolves Go module versions using a GOPROXY
type Resolver struct {
	proxyURLs  []string // List of proxy URLs in priority order
	httpClient *http.Client
}

// NewResolver creates a new module version resolver
func NewResolver(proxyURL string) *Resolver {
	if proxyURL == "" {
		proxyURL = "https://proxy.golang.org"
	}

	// GOPROXY can be a comma-separated list like "proxy1,proxy2,direct"
	// Filter to only HTTP(S) proxies
	proxies := strings.Split(proxyURL, ",")
	validProxies := make([]string, 0, len(proxies))

	for _, proxy := range proxies {
		proxy = strings.TrimSpace(proxy)
		if strings.HasPrefix(proxy, "http://") || strings.HasPrefix(proxy, "https://") {
			validProxies = append(validProxies, proxy)
		}
	}

	// Fallback to default if no valid proxies found
	if len(validProxies) == 0 {
		validProxies = []string{"https://proxy.golang.org"}
	}

	return &Resolver{
		proxyURLs: validProxies,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// ModuleInfo contains information about a module version
type ModuleInfo struct {
	Version string    `json:"Version"` // version string
	Time    time.Time `json:"Time"`    // commit time
}

// ResolveVersion resolves a module version (e.g., "latest", "v1", "v1.2") to an exact version
// Returns the exact version string (e.g., "v1.2.3") or an error
func (r *Resolver) ResolveVersion(ctx context.Context, modulePath, version string) (string, error) {
	// If version is already a full semantic version, return it as-is
	if version != "" && version != "latest" && strings.Count(version, ".") >= 2 {
		// Looks like a full version already (e.g., v1.2.3)
		return version, nil
	}

	// If no version specified, use "latest"
	if version == "" {
		version = "latest"
	}

	// Escape the module path for URL
	escapedPath, err := module.EscapePath(modulePath)
	if err != nil {
		return "", fmt.Errorf("invalid module path: %w", err)
	}

	// Try each proxy in order until one succeeds
	var lastErr error
	for _, proxyURL := range r.proxyURLs {
		// Construct the version info endpoint URL
		// For "latest", use @latest (no .info suffix)
		// For specific versions, use @v/version.info
		var infoURL string
		if version == "latest" {
			infoURL = fmt.Sprintf("%s/%s/@latest", proxyURL, escapedPath)
		} else {
			infoURL = fmt.Sprintf("%s/%s/@v/%s.info", proxyURL, escapedPath, version)
		}

		// Make HTTP request
		req, err := http.NewRequestWithContext(ctx, "GET", infoURL, nil)
		if err != nil {
			lastErr = fmt.Errorf("failed to create request: %w", err)
			continue
		}

		resp, err := r.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("proxy %s: %w", proxyURL, err)
			continue
		}

		// Check status
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
			resp.Body.Close()
			lastErr = fmt.Errorf("module %s@%s not found on proxy %s", modulePath, version, proxyURL)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("proxy %s returned status %d: %s", proxyURL, resp.StatusCode, string(body))
			continue
		}

		// Parse the JSON response
		var info ModuleInfo
		if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
			resp.Body.Close()
			lastErr = fmt.Errorf("proxy %s: failed to parse version info: %w", proxyURL, err)
			continue
		}
		resp.Body.Close()

		// Success!
		return info.Version, nil
	}

	// All proxies failed
	if lastErr != nil {
		return "", fmt.Errorf("all proxies failed for %s@%s: %w", modulePath, version, lastErr)
	}
	return "", fmt.Errorf("no proxies available")
}

// ResolveVersions resolves multiple module versions in parallel
// Returns a map of module paths to resolved versions
func (r *Resolver) ResolveVersions(ctx context.Context, modules map[string]string) (map[string]string, error) {
	type result struct {
		module  string
		version string
		err     error
	}

	results := make(chan result, len(modules))

	// Resolve in parallel
	for modulePath, version := range modules {
		go func(mod, ver string) {
			resolved, err := r.ResolveVersion(ctx, mod, ver)
			results <- result{module: mod, version: resolved, err: err}
		}(modulePath, version)
	}

	// Collect results
	resolved := make(map[string]string, len(modules))
	var errs []error

	for i := 0; i < len(modules); i++ {
		res := <-results
		if res.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", res.module, res.err))
		} else {
			resolved[res.module] = res.version
		}
	}

	if len(errs) > 0 {
		// Return first error (could be improved to return all)
		return nil, errs[0]
	}

	return resolved, nil
}
