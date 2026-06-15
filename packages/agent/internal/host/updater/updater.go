// Package updater handles auto-update: check, download, Ed25519 verify, atomic swap, rollback.
package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/hub"
)

// Config holds updater settings.
type Config struct {
	Enabled       bool
	CheckInterval time.Duration
	PublicKey     ed25519.PublicKey // Ed25519 public key for signature verification
	// DataDir is the directory where the updater persists the version floor file
	// (updater-floor.json). When empty, the version floor is not persisted and
	// only the in-memory current version is used as the monotonicity lower bound.
	DataDir string
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

	// Verify manifest signature BEFORE downloading the binary (fail fast).
	// The manifest signature binds version + sha256 + downloadURL into a single
	// signed tuple, preventing mix-and-match replay of an old signed binary at a
	// new version string (or vice versa). Scheme:
	//   Ed25519.Sign( sha256( version + ":" + sha256hex + ":" + downloadURL ) )
	if info.Signature == "" {
		return false, fmt.Errorf("update rejected: server did not provide Ed25519 signature")
	}
	if u.cfg.PublicKey == nil {
		return false, fmt.Errorf("update rejected: no Ed25519 public key configured")
	}
	if err := verifyManifestSignature(info, u.cfg.PublicKey); err != nil {
		return false, fmt.Errorf("manifest signature verification failed: %w", err)
	}

	// Version monotonicity check: reject downgrades and same-version installs.
	// Also enforce the persisted version floor (written after every successful
	// install) so an attacker cannot replay an older legitimately-signed release
	// even if the binary itself passes verification.
	if !semverGT(info.Version, u.version) {
		return false, fmt.Errorf("update rejected: version %s does not supersede current %s", info.Version, u.version)
	}
	floor, floorErr := u.readVersionFloor()
	if floorErr == nil && floor != "" && !semverGT(info.Version, floor) {
		return false, fmt.Errorf("update rejected: version %s does not supersede installed floor %s", info.Version, floor)
	}

	// Download. On macOS the release artifact is a whole-bundle .pkg (app +
	// daemon + NE extension + embedded launchd plist), installed with
	// /usr/sbin/installer so the swap is atomic across the whole bundle and the
	// pkg's postinstall re-registers SMAppService + re-activates the NE. On other
	// platforms it is the bare daemon binary, swapped in place.
	tmpPath := u.binaryPath + ".tmp"
	if u.osName == "darwin" {
		tmpPath = u.binaryPath + ".update.pkg"
	}
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

	// Verify Ed25519 binary signature (defense-in-depth second check after manifest sig).
	// BinarySignature signs sha256(binary_bytes) — confirms the downloaded file content
	// is exactly the binary the release pipeline produced, independently of the manifest.
	if info.BinarySignature == "" {
		os.Remove(tmpPath) //nolint:errcheck // best-effort: download tmp; OS will purge regardless
		return false, fmt.Errorf("update rejected: server did not provide binary Ed25519 signature")
	}
	if err := u.verifySignature(tmpPath, info.BinarySignature); err != nil {
		os.Remove(tmpPath) //nolint:errcheck // best-effort: download tmp; OS will purge regardless
		return false, fmt.Errorf("binary signature verification failed: %w", err)
	}

	// Apply: whole-bundle .pkg install on macOS, in-place binary swap elsewhere.
	if err := u.applyUpdate(tmpPath); err != nil {
		os.Remove(tmpPath) //nolint:errcheck // best-effort: download tmp; OS will purge regardless
		return false, fmt.Errorf("apply update: %w", err)
	}

	// Persist the version floor so future checks cannot roll back below this version.
	if err := u.writeVersionFloor(info.Version); err != nil {
		// Non-fatal: floor persistence is defense-in-depth. The current version
		// check already blocks an immediate downgrade; the floor is an extra guard
		// across daemon restarts. Log and continue.
		slog.Warn("update applied but version floor could not be persisted", "version", info.Version, "error", err)
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

// verifyManifestSignature verifies that the manifest fields (version, sha256hex, downloadURL)
// were signed together by the release key. This prevents an attacker from mixing a
// legitimately-signed binary with a different version string or download URL.
//
// Signed message: sha256( version + ":" + sha256hex + ":" + downloadURL )
// Signature encoding: base64 (StdEncoding) Ed25519 signature over the sha256 digest bytes.
func verifyManifestSignature(info hub.UpdateInfo, pubKey ed25519.PublicKey) error {
	sigBytes, err := base64.StdEncoding.DecodeString(info.Signature)
	if err != nil {
		return fmt.Errorf("decode manifest signature: %w", err)
	}

	canonical := info.Version + ":" + info.SHA256 + ":" + info.DownloadURL
	digest := sha256.Sum256([]byte(canonical))

	if !ed25519.Verify(pubKey, digest[:], sigBytes) {
		return fmt.Errorf("Ed25519 manifest signature invalid")
	}
	return nil
}

// verifySignature verifies the Ed25519 binary signature (defense-in-depth).
// Signs sha256(binary_bytes) — confirms the downloaded file content is the
// exact binary that was hashed and signed at release time.
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

// installerCommand builds the platform package-install invocation. It is a
// package var (defined per-GOOS in updater_darwin.go / updater_other.go) so
// tests can dispatch the .pkg path without launching the real installer. The
// macOS system path lives in the build-tagged darwin file to keep this
// cross-platform file free of hardcoded OS paths.

// applyUpdate installs the verified artifact: whole-bundle .pkg on macOS, an
// in-place binary rename everywhere else.
func (u *Updater) applyUpdate(artifactPath string) error {
	if u.osName == "darwin" {
		return u.pkgInstallDarwin(artifactPath)
	}
	return u.atomicSwap(artifactPath)
}

// pkgInstallDarwin runs `installer -pkg <pkg> -target /` DETACHED (new session,
// not waited on): the pkg's preinstall boots out the running daemon — this very
// process — so a child would die mid-install, but a detached session survives,
// lays down the bundle, and runs the postinstall that re-opens the app
// (SMAppService re-register + NE re-activate). Returns once launched; the new
// version reporting in is the observable result.
func (u *Updater) pkgInstallDarwin(pkgPath string) error {
	if _, err := os.Stat(pkgPath); err != nil {
		return fmt.Errorf("update pkg not found: %w", err)
	}
	cmd := installerCommand(pkgPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // outlive the daemon bootout
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("launch installer: %w", err)
	}
	_ = cmd.Process.Release() // reaped by launchd/init after we exit; do not Wait
	slog.Info("whole-bundle update: detached installer launched", "pkg", pkgPath)
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

// versionFloor is the JSON structure persisted in updater-floor.json.
type versionFloor struct {
	Version string `json:"version"`
}

// versionFloorPath returns the path of the version floor file, or "" if DataDir is unset.
func (u *Updater) versionFloorPath() string {
	if u.cfg.DataDir == "" {
		return ""
	}
	return filepath.Join(u.cfg.DataDir, "updater-floor.json")
}

// readVersionFloor returns the persisted version floor, or "" if absent / DataDir unset.
func (u *Updater) readVersionFloor() (string, error) {
	p := u.versionFloorPath()
	if p == "" {
		return "", nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var f versionFloor
	if err := json.Unmarshal(data, &f); err != nil {
		return "", fmt.Errorf("parse version floor: %w", err)
	}
	return f.Version, nil
}

// writeVersionFloor persists the installed version as the new floor.
// Subsequent checks will reject any manifest whose version does not exceed this floor.
func (u *Updater) writeVersionFloor(version string) error {
	p := u.versionFloorPath()
	if p == "" {
		return nil // DataDir not configured — floor persistence is skipped
	}
	data, err := json.Marshal(versionFloor{Version: version})
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0600)
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

// semverGT returns true if version a is strictly greater than version b.
// Both must be "vMAJOR.MINOR.PATCH" or "MAJOR.MINOR.PATCH" format (leading 'v' stripped).
// Non-numeric segments fall back to lexicographic comparison of the full string.
func semverGT(a, b string) bool {
	a = strings.TrimPrefix(a, "v")
	b = strings.TrimPrefix(b, "v")
	aParts := strings.SplitN(a, ".", 3)
	bParts := strings.SplitN(b, ".", 3)
	// Pad to 3 parts for comparison.
	for len(aParts) < 3 {
		aParts = append(aParts, "0")
	}
	for len(bParts) < 3 {
		bParts = append(bParts, "0")
	}
	for i := range 3 {
		ai, aerr := strconv.Atoi(aParts[i])
		bi, berr := strconv.Atoi(bParts[i])
		if aerr != nil || berr != nil {
			// Non-numeric: fall back to lexicographic on full strings.
			return a > b
		}
		if ai != bi {
			return ai > bi
		}
	}
	return false // equal
}
