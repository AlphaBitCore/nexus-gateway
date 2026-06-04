package infra

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/template"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/platform/hub"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/identity/iam"
	"github.com/AlphaBitCore/nexus-gateway/packages/shared/schemas/configkey"
	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

// RegisterSetupRoutes registers the setup guide endpoints under
// /api/admin/setup/proxy/:thingId/...
func (h *Handler) RegisterSetupRoutes(g *echo.Group, iamMW func(action string) echo.MiddlewareFunc) {
	p := g.Group("/setup/proxy/:thingId", iamMW(iam.ResourceSettings.Action(iam.VerbRead)))
	p.GET("/ca-cert", h.SetupGetCACert)
	p.GET("/mdm-profile", h.SetupGetMDMProfile)
	p.GET("/pac-file", h.SetupGetPACFile)
	p.PATCH("/onboarding", h.SetupPatchOnboarding, iamMW(iam.ResourceSettings.Action(iam.VerbWrite)))
}

// defaultSetupHTTPClient is used for forwarding requests to the compliance proxy.
var defaultSetupHTTPClient = nexushttp.New(nexushttp.Config{
	Timeout:        15 * time.Second,
	Caller:         "cp-setup-relay",
	PropagateReqID: true,
})

// setupClient returns the HTTP client to use for setup relay calls.
func (h *Handler) setupClient() *http.Client {
	if h.complianceProxyClient != nil {
		return h.complianceProxyClient
	}
	return defaultSetupHTTPClient
}

// resolveManagementURL fetches the proxy's management base URL from Hub.
// Returns a 404 JSON error if the thing is unknown or has no management URL.
func (h *Handler) resolveManagementURL(c echo.Context, thingID string) (string, error) {
	meta, err := h.hub.GetThingServiceMeta(c.Request().Context(), thingID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return "", c.JSON(http.StatusNotFound, map[string]any{
				"error":   "thing_not_found",
				"message": "No compliance proxy Thing with the given ID exists",
			})
		}
		h.logger.Warn("setup: Hub GetThingServiceMeta failed", "thingId", thingID, "error", err)
		return "", c.JSON(http.StatusBadGateway, map[string]any{
			"error":   "hub_error",
			"message": "Could not reach Hub to resolve proxy management URL",
		})
	}
	if meta.ManagementURL == "" {
		return "", c.JSON(http.StatusNotFound, map[string]any{
			"error":   "no_management_url",
			"message": "The proxy Thing has not yet reported its managementURL",
		})
	}
	return meta.ManagementURL, nil
}

// SetupGetCACert handles GET /api/admin/setup/proxy/:thingId/ca-cert.
// Relays GET {managementURL}/management/ca-cert to the live proxy instance.
func (h *Handler) SetupGetCACert(c echo.Context) error {
	thingID := c.Param("thingId")
	managementBase, err := h.resolveManagementURL(c, thingID)
	if err != nil {
		return err
	}

	targetURL := strings.TrimRight(managementBase, "/") + "/management/ca-cert"
	req, err := http.NewRequestWithContext(c.Request().Context(), http.MethodGet, targetURL, nil)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{
			"error":   "relay_error",
			"message": fmt.Sprintf("build relay request: %v", err),
		})
	}

	resp, err := h.setupClient().Do(req)
	if err != nil {
		h.logger.Warn("setup: relay ca-cert failed", "thingId", thingID, "target", targetURL, "error", err)
		return c.JSON(http.StatusBadGateway, map[string]any{
			"error":   "proxy_unreachable",
			"message": "Timeout connecting to proxy management endpoint",
		})
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return c.JSON(http.StatusBadGateway, map[string]any{
			"error":   "proxy_unreachable",
			"message": "Failed to read response from proxy management endpoint",
		})
	}
	if resp.StatusCode != http.StatusOK {
		return c.JSON(http.StatusBadGateway, map[string]any{
			"error":   "proxy_error",
			"message": fmt.Sprintf("proxy management endpoint returned %d", resp.StatusCode),
		})
	}

	c.Response().Header().Set("Content-Disposition", `attachment; filename="nexus-proxy-ca.crt"`)
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.Blob(http.StatusOK, "application/x-pem-file", body)
}

// mdmProfileTmpl is the Apple MDM .mobileconfig template with a CA cert payload.
// The Organization and CertB64 fields are injected per-request.
var mdmProfileTmpl = template.Must(template.New("mobileconfig").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>PayloadContent</key>
  <array>
    <dict>
      <key>PayloadCertificateFileName</key>
      <string>nexus-proxy-ca.crt</string>
      <key>PayloadContent</key>
      <data>{{.CertB64}}</data>
      <key>PayloadDescription</key>
      <string>Nexus Gateway Proxy Sub-CA certificate — required for AI traffic inspection</string>
      <key>PayloadDisplayName</key>
      <string>Nexus Gateway Proxy CA</string>
      <key>PayloadIdentifier</key>
      <string>com.nexus-gateway.proxy-ca.cert</string>
      <key>PayloadType</key>
      <string>com.apple.security.root</string>
      <key>PayloadUUID</key>
      <string>8E4E57D1-F2A0-4C16-A8B9-3E97B6C02D8A</string>
      <key>PayloadVersion</key>
      <integer>1</integer>
    </dict>
  </array>
  <key>PayloadDescription</key>
  <string>Installs the {{.Organization}} Nexus Gateway Proxy CA certificate into the System keychain as a trusted root.</string>
  <key>PayloadDisplayName</key>
  <string>{{.Organization}} Nexus Proxy CA Trust</string>
  <key>PayloadIdentifier</key>
  <string>com.nexus-gateway.proxy-ca</string>
  <key>PayloadOrganization</key>
  <string>{{.Organization}}</string>
  <key>PayloadRemovalDisallowed</key>
  <false/>
  <key>PayloadType</key>
  <string>Configuration</string>
  <key>PayloadUUID</key>
  <string>4B7C3A21-D9E0-4F82-B6C1-1A2E4F7D9B3C</string>
  <key>PayloadVersion</key>
  <integer>1</integer>
</dict>
</plist>
`))

// SetupGetMDMProfile handles GET /api/admin/setup/proxy/:thingId/mdm-profile.
// Fetches the CA cert from the proxy and renders an Apple MDM .mobileconfig.
func (h *Handler) SetupGetMDMProfile(c echo.Context) error {
	thingID := c.Param("thingId")
	organization := c.QueryParam("organization")
	if organization == "" {
		organization = "Nexus Gateway"
	}

	managementBase, err := h.resolveManagementURL(c, thingID)
	if err != nil {
		return err
	}

	// Fetch the CA cert PEM from the live proxy.
	targetURL := strings.TrimRight(managementBase, "/") + "/management/ca-cert"
	req, err := http.NewRequestWithContext(c.Request().Context(), http.MethodGet, targetURL, nil)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{
			"error": "relay_error", "message": fmt.Sprintf("build relay request: %v", err),
		})
	}
	resp, err := h.setupClient().Do(req)
	if err != nil {
		return c.JSON(http.StatusBadGateway, map[string]any{
			"error": "proxy_unreachable", "message": "Timeout connecting to proxy management endpoint",
		})
	}
	defer func() { _ = resp.Body.Close() }()
	certPEM, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		return c.JSON(http.StatusBadGateway, map[string]any{
			"error": "proxy_unreachable", "message": "Failed to read CA cert from proxy",
		})
	}

	certB64 := base64.StdEncoding.EncodeToString(certPEM)
	var buf bytes.Buffer
	if err := mdmProfileTmpl.Execute(&buf, struct {
		Organization string
		CertB64      string
	}{Organization: template.HTMLEscapeString(organization), CertB64: certB64}); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{
			"error": "template_error", "message": fmt.Sprintf("render MDM profile: %v", err),
		})
	}

	c.Response().Header().Set("Content-Disposition", `attachment; filename="nexus-proxy-ca.mobileconfig"`)
	return c.Blob(http.StatusOK, "application/x-apple-aspen-config", buf.Bytes())
}

// pacFileTmpl is the JavaScript PAC file template. Domains is a slice of
// dnsDomainIs(...) fragments for all AI provider domains.
var pacFileTmpl = template.Must(template.New("pac").Parse(`function FindProxyForURL(url, host) {
{{- range $i, $frag := .Fragments}}
    {{- if $i}} ||{{end}}
    if ({{$frag}})
{{- end}}
        return "PROXY {{.ProxyHost}}:{{.ProxyPort}}";
{{- if .FailOpen}}
    return "PROXY {{.ProxyHost}}:{{.ProxyPort}}; DIRECT";
{{- else}}
    return "DIRECT";
{{- end}}
}
`))

// SetupGetPACFile handles GET /api/admin/setup/proxy/:thingId/pac-file.
// Generates a PAC file routing all monitored AI provider domains via the proxy.
func (h *Handler) SetupGetPACFile(c echo.Context) error {
	proxyHost := c.QueryParam("proxyHost")
	proxyPort := c.QueryParam("proxyPort")
	if proxyHost == "" || proxyPort == "" {
		return c.JSON(http.StatusBadRequest, map[string]any{
			"error":   "invalid_params",
			"message": "proxyHost and proxyPort are required",
		})
	}

	failOpen := c.QueryParam("failOpen") == "true"

	domains, err := h.interception.ListEnabledInterceptionDomains(c.Request().Context())
	if err != nil {
		h.logger.Warn("setup: list interception domains failed", "error", err)
		return c.JSON(http.StatusInternalServerError, map[string]any{
			"error": "db_error", "message": "Could not load interception domains",
		})
	}

	var fragments []string
	for _, d := range domains {
		host := d.HostPattern
		// Strip leading wildcard (*.) for dnsDomainIs — dnsDomainIs(host, ".example.com")
		// matches sub.example.com but not example.com itself; we use isInNet for wildcards
		// and dnsDomainIs for exact matches.
		if strings.HasPrefix(host, "*.") {
			fragments = append(fragments, fmt.Sprintf(`dnsDomainIs(host, %q)`, host[1:]))
		} else {
			fragments = append(fragments, fmt.Sprintf(`host === %q || dnsDomainIs(host, ".%s")`, host, host))
		}
	}

	if len(fragments) == 0 {
		// No domains — return DIRECT-only PAC so clients don't fail.
		fragments = []string{`false`}
	}

	var buf bytes.Buffer
	if err := pacFileTmpl.Execute(&buf, struct {
		Fragments []string
		ProxyHost string
		ProxyPort string
		FailOpen  bool
	}{Fragments: fragments, ProxyHost: proxyHost, ProxyPort: proxyPort, FailOpen: failOpen}); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]any{
			"error": "template_error", "message": fmt.Sprintf("render PAC file: %v", err),
		})
	}

	c.Response().Header().Set("Content-Disposition", `attachment; filename="nexus-proxy.pac"`)
	return c.Blob(http.StatusOK, "application/x-ns-proxy-autoconfig", buf.Bytes())
}

// SetupPatchOnboarding handles PATCH /api/admin/setup/proxy/:thingId/onboarding.
// Pushes onboarding.enabled to the proxy Thing's shadow desired state via Hub.
func (h *Handler) SetupPatchOnboarding(c echo.Context) error {
	thingID := c.Param("thingId")

	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]any{
			"error": "invalid_params", "message": "request body must be {\"enabled\": bool}",
		})
	}

	_, err := h.hub.NotifyConfigChange(c.Request().Context(), hub.ConfigChangeRequest{
		ThingType: "compliance-proxy",
		ConfigKey: configkey.Onboarding,
		State:     map[string]any{"enabled": req.Enabled},
	})
	if err != nil {
		h.logger.Warn("setup: push onboarding shadow failed", "thingId", thingID, "error", err)
		return c.JSON(http.StatusBadGateway, map[string]any{
			"error": "hub_error", "message": "Could not push onboarding state to Hub",
		})
	}

	return c.JSON(http.StatusOK, map[string]any{
		"thingId":  thingID,
		"enabled":  req.Enabled,
		"pushedAt": time.Now().UTC().Format(time.RFC3339),
	})
}
