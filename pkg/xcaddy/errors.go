package xcaddy

import (
	"fmt"
	"strings"
)

// BuildError represents an error from the build process with metadata
type BuildError struct {
	Err    error  // The underlying error
	Output string // Command output that caused the error
	Fatal  bool   // If true, error is permanent and should not be retried
}

func (e *BuildError) Error() string {
	if e.Fatal {
		return fmt.Sprintf("fatal build error: %v", e.Err)
	}
	return fmt.Sprintf("build error: %v", e.Err)
}

func (e *BuildError) Unwrap() error {
	return e.Err
}

// IsFatalError returns true if the error is a fatal BuildError
func IsFatalError(err error) bool {
	if buildErr, ok := err.(*BuildError); ok {
		return buildErr.Fatal
	}
	return false
}

// classifyBuildError analyzes command output to determine if an error is fatal
func classifyBuildError(output string) bool {
	// Fatal errors that should not be retried
	// this is incredibly ai lmfao
	fatalPatterns := []string{
		// Module/version errors
		"no matching versions for query",
		"invalid version",
		"malformed module path",
		"module not found",
		"invalid pseudo-version",
		"invalid semantic version",
		"reading module.zip:",

		// Checksum/security errors
		"checksum mismatch",
		"SECURITY ERROR",
		"verifying module:",

		// Import/dependency errors
		"ambiguous import",
		"cannot find package",
		"no required module provides package",

		// Compilation errors (syntax, type errors, etc)
		"syntax error",
		"undefined:",
		"type error",
		"cannot use",
		"not enough arguments",
		"too many arguments",
		"undeclared name:",
		"imported but not used",

		// Go version incompatibility
		"requires go >=",
		"go.mod requires go",

		// Invalid replacements
		"replacement directory",
		"replacement not found",
	}

	outputLower := strings.ToLower(output)

	for _, pattern := range fatalPatterns {
		if strings.Contains(outputLower, strings.ToLower(pattern)) {
			return true
		}
	}

	// Transient/retryable errors (not fatal)
	transientPatterns := []string{
		"connection refused",
		"timeout",
		"temporary failure",
		"dial tcp: i/o timeout",
		"tls handshake timeout",
		"unexpected eof",
		"connection reset by peer",
		"no route to host",
		"network is unreachable",
		"too many open files",
		"resource temporarily unavailable",
	}

	for _, pattern := range transientPatterns {
		if strings.Contains(outputLower, strings.ToLower(pattern)) {
			return false // Explicitly not fatal
		}
	}

	// Default: unknown errors are not fatal (allow retry)
	return false
}
