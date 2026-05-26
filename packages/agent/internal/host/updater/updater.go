// Package updater handles auto-update: check, download, Ed25519 verify, atomic swap, rollback.
package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/hub"
)

// Config holds updater settings.
type Config struct {
	Enabled       bool
	CheckInterval time.Duration
	PublicKey     ed25519.PublicKey // Ed25519 public key for signature verification
}

// UpdateChecker is the minimal interface the updater needs from a Hub
// client. Defined at the consumer side for testability.
type UpdateChecker interface {
	CheckUpdate(ctx context.Context, currentVersion, osName string) (hub.UpdateInfo, error)
	HTTPClient() *http.Client
}

// Updater handles auto-update lifecycle.
type Updater struct {
	client       UpdateChecker
	downloadHTTP *http.Client // uses the shared mTLS + CA-pinned transport
	cfg          Config
	version      string
	osName       string
	binaryPath   string
}

// NewUpdater creates an auto-updater.
// If the updater is enabled but no Ed25519 public key is configured, the updater
// is forcibly disabled to prevent unsigned updates from being accepted.
func NewUpdater(client UpdateChecker, cfg Config, version, osName, binaryPath string) *Updater {
	if cfg.Enabled && cfg.PublicKey == nil {
		// Warn — not error. Auto-updater is not yet a shipped feature on
		// this build, so the absence of a configured public key is the
		// EXPECTED state for every agent in production, not a failure.
		slog.Warn("auto-updater disabled: no Ed25519 public key configured — updates will be rejected without signature verification")
		cfg.Enabled = false
	}
	return &Updater{
		client:       client,
		downloadHTTP: client.HTTPClient(),
		cfg:          cfg,
		version:      version,
		osName:       osName,
		binaryPath:   binaryPath,
	}
}

// CheckAndUpdate checks for updates and applies if available. Returns true if updated.
func (u *Updater) CheckAndUpdate(ctx context.Context) (bool, error) {
	if !u.cfg.Enabled {
		return false, nil
	}

	info, err := u.client.CheckUpdate(ctx, u.version, u.osName)
	if err != nil {
		return false, fmt.Errorf("check update: %w", err)
	}

	if !info.Available {
		return false, nil
	}

	slog.Info("update available", "current", u.version, "new", info.Version, "force", info.ForceUpdate)

	// Download
	tmpPath := u.binaryPath + ".tmp"
	if err := u.download(ctx, info.DownloadURL, tmpPath); err != nil {
		os.Remove(tmpPath) //nolint:errcheck // best-effort: download tmp; OS will purge regardless
		return false, fmt.Errorf("download: %w", err)
	}

	// Verify SHA-256 (mandatory)
	if info.SHA256 == "" {
		os.Remove(tmpPath) //nolint:errcheck // best-effort: download tmp; OS will purge regardless
		return false, fmt.Errorf("update rejected: server did not provide SHA256 hash")
	}
	hash, err := fileSHA256(tmpPath)
	if err != nil {
		os.Remove(tmpPath) //nolint:errcheck // best-effort: download tmp; OS will purge regardless
		return false, fmt.Errorf("sha256: %w", err)
	}
	if hash != info.SHA256 {
		os.Remove(tmpPath) //nolint:errcheck // best-effort: download tmp; OS will purge regardless
		return false, fmt.Errorf("sha256 mismatch: expected %s, got %s", info.SHA256, hash)
	}

	// Verify Ed25519 signature (mandatory)
	if info.Signature == "" {
		os.Remove(tmpPath) //nolint:errcheck // best-effort: download tmp; OS will purge regardless
		return false, fmt.Errorf("update rejected: server did not provide Ed25519 signature")
	}
	if u.cfg.PublicKey == nil {
		os.Remove(tmpPath) //nolint:errcheck // best-effort: download tmp; OS will purge regardless
		return false, fmt.Errorf("update rejected: no Ed25519 public key configured")
	}
	if err := u.verifySignature(tmpPath, info.Signature); err != nil {
		os.Remove(tmpPath) //nolint:errcheck // best-effort: download tmp; OS will purge regardless
		return false, fmt.Errorf("signature verification failed: %w", err)
	}

	// Atomic swap
	if err := u.atomicSwap(tmpPath); err != nil {
		os.Remove(tmpPath) //nolint:errcheck // best-effort: download tmp; OS will purge regardless
		return false, fmt.Errorf("atomic swap: %w", err)
	}

	slog.Info("update applied", "version", info.Version)
	return true, nil
}

// Run starts the periodic update check loop. Blocks until ctx is cancelled.
// Equivalent to RunWithAvailabilityCallback(ctx, nil) — kept for callers
// that don't need to surface availability to the UI.
func (u *Updater) Run(ctx context.Context) {
	u.RunWithAvailabilityCallback(ctx, nil)
}

// RunWithAvailabilityCallback is the same periodic loop as Run, but it
// notifies the caller after every check via availableFn(true/false) so
// the Dashboard "Update available — install" banner can light up even
// when auto-install is disabled (e.g. no Ed25519 public key is
// provisioned, which is the prod default today). Decouples the
// availability signal from the install action:
//   - availableFn is called on EVERY tick regardless of cfg.Enabled,
//     so the menu / Dashboard see fresh state.
//   - The install path still requires cfg.Enabled + a valid signature
//     chain. When auto-install is off the user installs manually via
//     the .pkg download.
//
// Pass nil for availableFn when the caller doesn't surface a banner.
func (u *Updater) RunWithAvailabilityCallback(ctx context.Context, availableFn func(bool)) {
	ticker := time.NewTicker(u.cfg.CheckInterval)
	defer ticker.Stop()

	// Fire one immediate check on start so the UI doesn't have to
	// wait a full CheckInterval (typically 1h) after boot to learn
	// about a pending update from the previous session.
	u.checkAvailabilityOnce(ctx, availableFn)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if u.cfg.Enabled {
				if updated, err := u.CheckAndUpdate(ctx); err != nil {
					slog.Error("update check failed", "error", err)
				} else if updated {
					slog.Info("update applied, restart required")
					// On a successful auto-install the binary
					// swap is complete — the next launchd
					// respawn will pick up the new bits.
					if availableFn != nil {
						availableFn(false)
					}
					return
				} else {
					u.checkAvailabilityOnce(ctx, availableFn)
				}
			} else {
				u.checkAvailabilityOnce(ctx, availableFn)
			}
		}
	}
}

// checkAvailabilityOnce queries Hub and forwards the Available flag to
// the caller-supplied callback. No download, no signature check — just
// the boolean signal. Errors are silent (logged at Debug) so a Hub
// outage doesn't spam the agent log on every poll.
func (u *Updater) checkAvailabilityOnce(ctx context.Context, availableFn func(bool)) {
	if availableFn == nil {
		return
	}
	info, err := u.client.CheckUpdate(ctx, u.version, u.osName)
	if err != nil {
		slog.Debug("update availability check failed", "error", err)
		return
	}
	availableFn(info.Available)
}

func (u *Updater) download(ctx context.Context, url, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := u.downloadHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: %d", resp.StatusCode)
	}

	f, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck

	_, err = io.Copy(f, resp.Body)
	return err
}

func (u *Updater) verifySignature(filePath, signatureB64 string) error {
	sigBytes, err := base64.StdEncoding.DecodeString(signatureB64)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}

	hash, err := fileSHA256(filePath)
	if err != nil {
		return err
	}
	hashBytes, _ := hex.DecodeString(hash)

	if !ed25519.Verify(u.cfg.PublicKey, hashBytes, sigBytes) {
		return fmt.Errorf("Ed25519 signature invalid")
	}
	return nil
}

func (u *Updater) atomicSwap(newBinaryPath string) error {
	rollbackPath := u.binaryPath + ".rollback"

	// Remove old rollback if exists
	_ = os.Remove(rollbackPath)

	// Rename current → rollback
	if err := os.Rename(u.binaryPath, rollbackPath); err != nil {
		return fmt.Errorf("backup current binary: %w", err)
	}

	// Rename new → current
	if err := os.Rename(newBinaryPath, u.binaryPath); err != nil {
		// Restore rollback
		_ = os.Rename(rollbackPath, u.binaryPath)
		return fmt.Errorf("install new binary: %w", err)
	}

	return nil
}

// DetectCrashLoop checks if the previous run crashed quickly and rolls back.
func DetectCrashLoop(binaryPath, statusFile string, threshold time.Duration) bool {
	data, err := os.ReadFile(statusFile)
	if err != nil {
		return false
	}

	lastStart, err := time.Parse(time.RFC3339, string(data))
	if err != nil {
		return false
	}

	if time.Since(lastStart) < threshold {
		rollbackPath := binaryPath + ".rollback"
		if _, err := os.Stat(rollbackPath); err == nil {
			slog.Warn("crash loop detected, rolling back", "lastStart", lastStart, "threshold", threshold)
			_ = os.Rename(rollbackPath, binaryPath)
			return true
		}
	}
	return false
}

// WriteStartStatus writes the current time to the status file for crash loop detection.
func WriteStartStatus(statusFile string) error {
	return os.WriteFile(statusFile, []byte(time.Now().Format(time.RFC3339)), 0600)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
