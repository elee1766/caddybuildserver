package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/elee1766/caddybuildserver/pkg/db"
	"github.com/elee1766/caddybuildserver/pkg/modversion"
	"github.com/elee1766/caddybuildserver/pkg/storage"
	"github.com/go-playground/validator/v10"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Server represents the API server
type Server struct {
	pool      *pgxpool.Pool
	storage   *storage.Storage
	resolver  *modversion.Resolver
	mux       *http.ServeMux
	logger    *slog.Logger
	validator *validator.Validate
}

// New creates a new API server
func New(pool *pgxpool.Pool, stor *storage.Storage, resolver *modversion.Resolver, logger *slog.Logger) *Server {
	s := &Server{
		pool:      pool,
		storage:   stor,
		resolver:  resolver,
		mux:       http.NewServeMux(),
		logger:    logger,
		validator: validator.New(),
	}

	s.routes()
	return s
}

// ServeHTTP implements http.Handler
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// routes sets up the API routes
func (s *Server) routes() {
	s.mux.HandleFunc("POST /api/build", s.handleBuildRequest)
	s.mux.HandleFunc("GET /api/build/{buildKey}", s.handleBuildStatus)
	s.mux.HandleFunc("GET /api/download/{buildKey}", s.handleDownload)
	s.mux.HandleFunc("GET /api/compat", s.handleCompatDownload)
	s.mux.HandleFunc("GET /health", s.handleHealth)
}

// Plugin represents a plugin with optional version
type Plugin struct {
	Module  string `json:"module" validate:"required"` // e.g., "github.com/caddy-dns/cloudflare"
	Version string `json:"version,omitempty"`          // e.g., "v0.0.1" or empty for latest
}

// Replacement represents a Go module replacement
type Replacement struct {
	Old        string `json:"old" validate:"required"`         // Module to replace, e.g., "github.com/caddyserver/caddy/v2"
	New        string `json:"new" validate:"required"`         // Replacement module or path, e.g., "github.com/user/caddy/v2"
	NewVersion string `json:"new_version" validate:"required"` // Version for replacement module (required)
}

// BuildRequest represents an incoming build request
type BuildRequest struct {
	CaddyVersion string        `json:"caddy_version" validate:"required"`
	Plugins      []Plugin      `json:"plugins" validate:"dive"`
	Replacements []Replacement `json:"replacements" validate:"dive"`
	OS           string        `json:"os" validate:"required"`
	Arch         string        `json:"arch" validate:"required"`
	Force        bool          `json:"force,omitempty"` // Force rebuild even if failed (resets attempts to 0)
}

// BuildResponse represents the response to a build request
type BuildResponse struct {
	BuildKey string         `json:"build_key"`
	Status   db.BuildStatus `json:"status"`
	Message  string         `json:"message,omitempty"`
}

// ErrorResponse represents an error response
type ErrorResponse struct {
	Error string `json:"error"`
}

// handleBuildRequest handles POST /api/build
func (s *Server) handleBuildRequest(w http.ResponseWriter, r *http.Request) {
	s.logger.Info("received build request",
		"method", r.Method,
		"path", r.URL.Path,
		"remote_addr", r.RemoteAddr,
	)

	var req BuildRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.logger.Error("failed to decode request body",
			"error", err.Error(),
			"remote_addr", r.RemoteAddr,
		)
		respondJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid request body"})
		return
	}

	s.logger.Info("build request parsed",
		"caddy_version", req.CaddyVersion,
		"plugin_count", len(req.Plugins),
		"os", req.OS,
		"arch", req.Arch,
	)

	// Validate request
	if err := s.validator.Struct(req); err != nil {
		s.logger.Warn("validation failed",
			"error", err.Error(),
			"remote_addr", r.RemoteAddr,
		)
		respondJSON(w, http.StatusBadRequest, ErrorResponse{Error: err.Error()})
		return
	}

	// Resolve plugin versions
	pluginStrings, err := s.resolvePlugins(r.Context(), req.Plugins)
	if err != nil {
		s.logger.Error("failed to resolve plugin versions",
			"error", err.Error(),
			"remote_addr", r.RemoteAddr,
		)
		respondJSON(w, http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("failed to resolve plugin versions: %v", err)})
		return
	}

	s.logger.Info("resolved plugin versions",
		"plugins", pluginStrings,
	)

	// Convert replacements to string format
	replacementStrings := make([]string, len(req.Replacements))
	for i, repl := range req.Replacements {
		// Format: old=new@version (version is required)
		replacementStrings[i] = repl.Old + "=" + repl.New + "@" + repl.NewVersion
	}

	// Create build record
	config := db.BuildConfig{
		CaddyVersion: req.CaddyVersion,
		Plugins:      pluginStrings,
		Replacements: replacementStrings,
		OS:           req.OS,
		Arch:         req.Arch,
	}
	build, err := db.CreateBuild(r.Context(), s.pool, config)
	if err != nil {
		s.logger.Error("failed to create build record",
			"error", err.Error(),
			"caddy_version", req.CaddyVersion,
			"os", req.OS,
			"arch", req.Arch,
		)
		respondJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "failed to create build"})
		return
	}

	s.logger.Info("build record created or found",
		"build_key", build.BuildKey,
		"status", string(build.Status),
		"attempt", build.Attempt,
		"max_attempts", build.MaxAttempts,
		"force", req.Force,
	)

	// If build already exists and is completed (and not forced), return immediately
	if build.Status == db.BuildStatusCompleted {
		s.logger.Info("build already completed",
			"build_key", build.BuildKey,
		)
		respondJSON(w, http.StatusOK, BuildResponse{
			BuildKey: build.BuildKey,
			Status:   build.Status,
			Message:  "build already exists",
		})
		return
	}

	// Handle force rebuild: reset failed or completed builds back to pending
	if req.Force && build.Status == db.BuildStatusFailed {
		s.logger.Info("force rebuild requested, resetting build attempts",
			"build_key", build.BuildKey,
			"previous_status", string(build.Status),
		)
		if err := db.ResetBuildAttempts(r.Context(), s.pool, build.BuildKey); err != nil {
			s.logger.Error("failed to reset build attempts",
				"build_key", build.BuildKey,
				"error", err.Error(),
			)
			respondJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "failed to reset build"})
			return
		}
		// Fetch updated build status
		build, err = db.GetBuildByKey(r.Context(), s.pool, build.BuildKey)
		if err != nil {
			s.logger.Error("failed to fetch build after reset",
				"build_key", build.BuildKey,
				"error", err.Error(),
			)
			respondJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "failed to fetch build status"})
			return
		}
		s.logger.Info("build reset to pending",
			"build_key", build.BuildKey,
		)
	}

	// If build is terminally failed (no more retries) and not forced, return error
	if build.Status == db.BuildStatusFailed && build.Attempt >= build.MaxAttempts {
		errorMsg := "build failed after maximum attempts"
		if build.ErrorMessage != nil {
			errorMsg = *build.ErrorMessage
		}
		s.logger.Error("build terminally failed",
			"build_key", build.BuildKey,
			"attempt", build.Attempt,
			"max_attempts", build.MaxAttempts,
			"error", errorMsg,
		)
		respondJSON(w, http.StatusInternalServerError, ErrorResponse{Error: errorMsg})
		return
	}

	// If build is pending, building, or failed with retries remaining, return in-progress status
	if build.Status == db.BuildStatusPending || build.Status == db.BuildStatusBuilding || build.Status == db.BuildStatusFailed {
		var message string
		if build.Status == db.BuildStatusFailed {
			message = fmt.Sprintf("build failed, retrying (attempt %d/%d)", build.Attempt+1, build.MaxAttempts)
		} else {
			message = "build in progress"
		}

		s.logger.Info("build in progress or retrying",
			"build_key", build.BuildKey,
			"status", string(build.Status),
			"attempt", build.Attempt,
			"max_attempts", build.MaxAttempts,
		)
		respondJSON(w, http.StatusAccepted, BuildResponse{
			BuildKey: build.BuildKey,
			Status:   build.Status,
			Message:  message,
		})
		return
	}

	// Shouldn't reach here, but handle gracefully
	s.logger.Warn("unexpected build status",
		"build_key", build.BuildKey,
		"status", string(build.Status),
	)
	respondJSON(w, http.StatusAccepted, BuildResponse{
		BuildKey: build.BuildKey,
		Status:   build.Status,
		Message:  "build queued",
	})
}

// handleBuildStatus handles GET /api/build/{buildKey}
func (s *Server) handleBuildStatus(w http.ResponseWriter, r *http.Request) {
	buildKey := r.PathValue("buildKey")

	s.logger.Info("received build status request",
		"method", r.Method,
		"path", r.URL.Path,
		"build_key", buildKey,
		"remote_addr", r.RemoteAddr,
	)

	if buildKey == "" {
		s.logger.Warn("validation failed: missing build_key",
			"remote_addr", r.RemoteAddr,
		)
		respondJSON(w, http.StatusBadRequest, ErrorResponse{Error: "build_key is required"})
		return
	}

	build, err := db.GetBuildByKey(r.Context(), s.pool, buildKey)
	if err != nil {
		s.logger.Error("build not found",
			"error", err.Error(),
			"build_key", buildKey,
		)
		respondJSON(w, http.StatusNotFound, ErrorResponse{Error: "build not found"})
		return
	}

	s.logger.Info("build status retrieved successfully",
		"build_key", buildKey,
		"status", string(build.Status),
		"status_code", http.StatusOK,
	)

	respondJSON(w, http.StatusOK, build)
}

// handleDownload handles GET /api/download/{buildKey}
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	buildKey := r.PathValue("buildKey")

	s.logger.Info("received download request",
		"method", r.Method,
		"path", r.URL.Path,
		"build_key", buildKey,
		"remote_addr", r.RemoteAddr,
	)

	if buildKey == "" {
		s.logger.Warn("validation failed: missing build_key",
			"remote_addr", r.RemoteAddr,
		)
		respondJSON(w, http.StatusBadRequest, ErrorResponse{Error: "build_key is required"})
		return
	}

	build, err := db.GetBuildByKey(r.Context(), s.pool, buildKey)
	if err != nil {
		s.logger.Error("build not found",
			"error", err.Error(),
			"build_key", buildKey,
		)
		respondJSON(w, http.StatusNotFound, ErrorResponse{Error: "build not found"})
		return
	}

	// Check if build failed
	if build.Status == db.BuildStatusFailed {
		errorMsg := "build failed"
		if build.ErrorMessage != nil {
			errorMsg = *build.ErrorMessage
		}
		s.logger.Error("build failed",
			"build_key", buildKey,
			"error", errorMsg,
		)
		respondJSON(w, http.StatusInternalServerError, ErrorResponse{Error: errorMsg})
		return
	}

	// Check if build is not completed yet (pending or building)
	if build.Status != db.BuildStatusCompleted {
		s.logger.Warn("build not ready for download",
			"build_key", buildKey,
			"status", string(build.Status),
		)
		respondJSON(w, http.StatusAccepted, ErrorResponse{Error: fmt.Sprintf("build in progress (status: %s)", build.Status)})
		return
	}

	// Compute storage key from build key (no need to store it in DB)
	// Format: builds/{first_byte}/{build_key}
	prefix := buildKey[:2]
	storageKey := fmt.Sprintf("builds/%s/%s", prefix, buildKey)

	s.serveBuildFile(w, r, build, storageKey)
}

// handleCompatDownload handles GET /api/compat - Caddy server API compatibility endpoint
// Query parameters: os, arch, p (plugins, can be multiple), idempotency (optional)
func (s *Server) handleCompatDownload(w http.ResponseWriter, r *http.Request) {
	s.logger.Info("received compat download request",
		"method", r.Method,
		"path", r.URL.Path,
		"query", r.URL.RawQuery,
		"remote_addr", r.RemoteAddr,
	)

	// Parse query parameters
	query := r.URL.Query()
	osParam := query.Get("os")
	archParam := query.Get("arch")
	plugins := query["p"]      // Can be multiple
	replacements := query["r"] // Can be multiple
	caddyVersion := query.Get("version")
	if caddyVersion == "" {
		caddyVersion = "latest"
	}

	// Ignore deprecated 'd' flag (debug builds, now a no-op for backward compatibility)
	_ = query.Get("d")

	// Validate required parameters
	if osParam == "" {
		s.logger.Warn("validation failed: missing os parameter",
			"remote_addr", r.RemoteAddr,
		)
		respondJSON(w, http.StatusBadRequest, ErrorResponse{Error: "os parameter is required"})
		return
	}
	if archParam == "" {
		s.logger.Warn("validation failed: missing arch parameter",
			"remote_addr", r.RemoteAddr,
		)
		respondJSON(w, http.StatusBadRequest, ErrorResponse{Error: "arch parameter is required"})
		return
	}

	s.logger.Info("compat download request parsed",
		"caddy_version", caddyVersion,
		"plugin_count", len(plugins),
		"os", osParam,
		"arch", archParam,
	)

	// Resolve plugin versions from string format "module" or "module@version"
	pluginStructs := make([]Plugin, 0, len(plugins))
	for _, p := range plugins {
		parts := strings.SplitN(p, "@", 2)
		plugin := Plugin{Module: strings.TrimSpace(parts[0])}
		if len(parts) == 2 {
			plugin.Version = strings.TrimSpace(parts[1])
		}
		if plugin.Module != "" {
			pluginStructs = append(pluginStructs, plugin)
		}
	}

	pluginStrings, err := s.resolvePlugins(r.Context(), pluginStructs)
	if err != nil {
		s.logger.Error("failed to resolve plugin versions",
			"error", err.Error(),
			"remote_addr", r.RemoteAddr,
		)
		respondJSON(w, http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("failed to resolve plugin versions: %v", err)})
		return
	}

	s.logger.Info("resolved plugin versions for compat request",
		"plugins", pluginStrings,
	)

	// Parse replacements from string format "old=new@version" (version is required)
	// We keep them as-is since they're already in the internal format
	replacementStrings := make([]string, 0, len(replacements))
	for _, replStr := range replacements {
		parts := strings.SplitN(replStr, "=", 2)
		if len(parts) != 2 {
			s.logger.Warn("invalid replacement format",
				"replacement", replStr,
				"remote_addr", r.RemoteAddr,
			)
			respondJSON(w, http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("invalid replacement format: %s (expected 'old=new@version')", replStr)})
			return
		}
		old := strings.TrimSpace(parts[0])
		newPart := strings.TrimSpace(parts[1])
		if old == "" || newPart == "" {
			s.logger.Warn("invalid replacement: old or new is empty",
				"replacement", replStr,
				"remote_addr", r.RemoteAddr,
			)
			respondJSON(w, http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("invalid replacement: %s (old and new cannot be empty)", replStr)})
			return
		}
		// Validate that version is present (must contain @)
		if !strings.Contains(newPart, "@") {
			s.logger.Warn("invalid replacement: version is required",
				"replacement", replStr,
				"remote_addr", r.RemoteAddr,
			)
			respondJSON(w, http.StatusBadRequest, ErrorResponse{Error: fmt.Sprintf("invalid replacement: %s (version is required, use format 'old=new@version')", replStr)})
			return
		}
		// Keep the original format with @version
		replacementStrings = append(replacementStrings, old+"="+newPart)
	}

	// Create or find existing build
	config := db.BuildConfig{
		CaddyVersion: caddyVersion,
		Plugins:      pluginStrings,
		Replacements: replacementStrings,
		OS:           osParam,
		Arch:         archParam,
	}
	build, err := db.CreateBuild(r.Context(), s.pool, config)
	if err != nil {
		s.logger.Error("failed to create build record",
			"error", err.Error(),
			"caddy_version", caddyVersion,
			"os", osParam,
			"arch", archParam,
		)
		respondJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "failed to create build"})
		return
	}

	s.logger.Info("build record created or found",
		"build_key", build.BuildKey,
		"status", string(build.Status),
		"attempt", build.Attempt,
		"max_attempts", build.MaxAttempts,
	)

	// If already completed, serve immediately
	if build.Status == db.BuildStatusCompleted && build.StorageKey != nil {
		s.serveBuildFile(w, r, build, *build.StorageKey)
		return
	}

	// If build is terminally failed (no more retries), return error
	if build.Status == db.BuildStatusFailed && build.Attempt >= build.MaxAttempts {
		errorMsg := "build failed after maximum attempts"
		if build.ErrorMessage != nil {
			errorMsg = *build.ErrorMessage
		}
		s.logger.Error("build terminally failed",
			"build_key", build.BuildKey,
			"attempt", build.Attempt,
			"max_attempts", build.MaxAttempts,
			"error", errorMsg,
		)
		respondJSON(w, http.StatusInternalServerError, ErrorResponse{Error: errorMsg})
		return
	}

	// Wait for build to complete (poll with timeout)
	ctx := r.Context()
	pollInterval := 2 * time.Second
	timeout := 30 * time.Minute
	deadline := time.Now().Add(timeout)

	s.logger.Info("waiting for build to complete",
		"build_key", build.BuildKey,
		"timeout", timeout,
	)

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.Warn("client disconnected while waiting for build",
				"build_key", build.BuildKey,
			)
			respondJSON(w, http.StatusRequestTimeout, ErrorResponse{Error: "request cancelled"})
			return
		case <-ticker.C:
			// Check if we've exceeded timeout
			if time.Now().After(deadline) {
				s.logger.Error("build timeout exceeded",
					"build_key", build.BuildKey,
					"timeout", timeout,
				)
				respondJSON(w, http.StatusRequestTimeout, ErrorResponse{Error: "build timeout exceeded"})
				return
			}

			// Poll build status
			build, err = db.GetBuildByKey(ctx, s.pool, build.BuildKey)
			if err != nil {
				s.logger.Error("failed to get build status",
					"error", err.Error(),
					"build_key", build.BuildKey,
				)
				respondJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "failed to get build status"})
				return
			}

			// Check build status
			switch build.Status {
			case db.BuildStatusCompleted:
				if build.StorageKey == nil {
					s.logger.Error("storage key not set for completed build",
						"build_key", build.BuildKey,
					)
					respondJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "storage key not set"})
					return
				}
				s.logger.Info("build completed, serving file",
					"build_key", build.BuildKey,
				)
				s.serveBuildFile(w, r, build, *build.StorageKey)
				return
			case db.BuildStatusFailed:
				errorMsg := "build failed"
				if build.ErrorMessage != nil {
					errorMsg = *build.ErrorMessage
				}
				s.logger.Error("build failed",
					"build_key", build.BuildKey,
					"error", errorMsg,
				)
				respondJSON(w, http.StatusInternalServerError, ErrorResponse{Error: errorMsg})
				return
			default:
				// Still pending or building, continue polling
				s.logger.Debug("build still in progress",
					"build_key", build.BuildKey,
					"status", string(build.Status),
				)
				continue
			}
		}
	}
}

// serveBuildFile retrieves and serves a build file from storage with signature if available
func (s *Server) serveBuildFile(w http.ResponseWriter, r *http.Request, build *db.Build, storageKey string) {
	// Determine filename
	filename := "caddy"
	if build.OS == "windows" {
		filename = "caddy.exe"
	}

	// Get binary file from storage
	binaryFile, err := s.storage.Get(r.Context(), storageKey)
	if err != nil {
		s.logger.Error("failed to retrieve file from storage",
			"error", err.Error(),
			"build_key", build.BuildKey,
			"storage_key", storageKey,
		)
		respondJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "failed to retrieve file"})
		return
	}
	defer binaryFile.Close()

	// If no signature, serve binary directly
	if !build.HasSignature {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))

		s.logger.Info("serving build file (no signature)",
			"build_key", build.BuildKey,
			"filename", filename,
			"storage_key", storageKey,
		)

		http.ServeContent(w, r, filename, *build.CompletedAt, binaryFile)
		return
	}

	// Get signature file from storage
	signatureKey := storageKey + ".asc"
	signatureFile, err := s.storage.Get(r.Context(), signatureKey)
	if err != nil {
		s.logger.Error("failed to retrieve signature from storage",
			"error", err.Error(),
			"build_key", build.BuildKey,
			"signature_key", signatureKey,
		)
		respondJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "failed to retrieve signature"})
		return
	}
	defer signatureFile.Close()

	// Serve as multipart response with both signature and binary
	s.logger.Info("serving build file with signature",
		"build_key", build.BuildKey,
		"filename", filename,
		"storage_key", storageKey,
		"signature_key", signatureKey,
	)

	writer := multipart.NewWriter(w)
	w.Header().Set("Content-Type", writer.FormDataContentType())

	// Write signature first
	signaturePart, err := writer.CreateFormFile("signature", filename+".asc")
	if err != nil {
		s.logger.Error("failed to create signature part",
			"error", err.Error(),
			"build_key", build.BuildKey,
		)
		respondJSON(w, http.StatusInternalServerError, ErrorResponse{Error: "failed to create multipart response"})
		return
	}

	_, err = io.Copy(signaturePart, signatureFile)
	if err != nil {
		s.logger.Error("failed to write signature",
			"error", err.Error(),
			"build_key", build.BuildKey,
		)
		return
	}

	// Write binary artifact
	artifactPart, err := writer.CreateFormFile("artifact", filename)
	if err != nil {
		s.logger.Error("failed to create artifact part",
			"error", err.Error(),
			"build_key", build.BuildKey,
		)
		return
	}

	_, err = io.Copy(artifactPart, binaryFile)
	if err != nil {
		s.logger.Error("failed to write artifact",
			"error", err.Error(),
			"build_key", build.BuildKey,
		)
		return
	}

	// Close the multipart writer
	err = writer.Close()
	if err != nil {
		s.logger.Error("failed to close multipart writer",
			"error", err.Error(),
			"build_key", build.BuildKey,
		)
	}
}

// handleHealth handles GET /health
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.logger.Info("received health check request",
		"method", r.Method,
		"path", r.URL.Path,
		"remote_addr", r.RemoteAddr,
	)

	// Check database connection
	if err := s.pool.Ping(context.Background()); err != nil {
		s.logger.Error("health check failed: database ping failed",
			"error", err.Error(),
		)
		respondJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unhealthy", "error": err.Error()})
		return
	}

	s.logger.Info("health check passed",
		"status_code", http.StatusOK,
	)

	respondJSON(w, http.StatusOK, map[string]string{"status": "healthy"})
}

// respondJSON writes a JSON response
func respondJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// resolvePlugins resolves plugin versions to their exact versions
// Returns plugins with resolved versions in format "module@v1.2.3"
func (s *Server) resolvePlugins(ctx context.Context, plugins []Plugin) ([]string, error) {
	if len(plugins) == 0 {
		return []string{}, nil
	}

	// Build map of module path to version for resolution
	toResolve := make(map[string]string)
	for _, plugin := range plugins {
		if plugin.Module == "" {
			continue
		}
		toResolve[plugin.Module] = plugin.Version
	}

	// Resolve all versions
	resolved, err := s.resolver.ResolveVersions(ctx, toResolve)
	if err != nil {
		return nil, err
	}

	// Build result list with resolved versions
	result := make([]string, 0, len(resolved))
	for modulePath, version := range resolved {
		result = append(result, modulePath+"@"+version)
	}

	return result, nil
}
