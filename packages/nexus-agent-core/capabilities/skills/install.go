package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// httpGetter fetches a URL. A seam so Install is unit-testable offline.
type httpGetter interface {
	Get(ctx context.Context, url string) (status int, body []byte, err error)
}

// stdHTTP is the real getter over net/http.
type stdHTTP struct{ client *http.Client }

func newStdHTTP() *stdHTTP { return &stdHTTP{client: http.DefaultClient} }

func (h *stdHTTP) Get(ctx context.Context, url string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, err
}

// InstallInfo is what the preview/install steps report for human review. Raw holds
// the exact downloaded bytes so InstallFetched can land precisely what was reviewed
// (no second download that could differ).
type InstallInfo struct {
	Name        string
	Description string
	SHA256      string
	Body        string
	Raw         []byte
}

// Preview downloads a skill over real HTTP and reports it for human review WITHOUT
// writing anything. The CLI shows the name/description/SHA-256/body and only lands
// it (InstallFetched) once the operator confirms — design §5.4's "download →
// checksum → human review → then usable", so an unreviewed skill is never active.
func Preview(ctx context.Context, url string) (InstallInfo, error) {
	return preview(ctx, newStdHTTP(), url)
}

// preview is the testable core of Preview (injected getter).
func preview(ctx context.Context, h httpGetter, url string) (InstallInfo, error) {
	status, body, err := h.Get(ctx, url)
	if err != nil {
		return InstallInfo{}, fmt.Errorf("download %s: %w", url, err)
	}
	if status != http.StatusOK {
		return InstallInfo{}, fmt.Errorf("download %s: HTTP %d", url, status)
	}
	sk, err := parseSkill(body)
	if err != nil {
		return InstallInfo{}, fmt.Errorf("downloaded file is not a valid skill: %w", err)
	}
	sum := sha256.Sum256(body)
	return InstallInfo{
		Name: sk.Name, Description: sk.Description,
		SHA256: hex.EncodeToString(sum[:]), Body: sk.Body, Raw: body,
	}, nil
}

// InstallFetched lands a previewed skill into localDir. Call only after the
// operator has reviewed the Preview output.
func InstallFetched(info InstallInfo, localDir string) (string, error) {
	if info.Name == "" || len(info.Raw) == 0 {
		return "", fmt.Errorf("nothing to install (preview the skill first)")
	}
	if err := os.MkdirAll(localDir, 0o700); err != nil {
		return "", fmt.Errorf("create skill dir: %w", err)
	}
	dest := filepath.Join(localDir, info.Name+".md")
	if err := os.WriteFile(dest, info.Raw, 0o644); err != nil {
		return "", fmt.Errorf("write skill: %w", err)
	}
	return dest, nil
}
