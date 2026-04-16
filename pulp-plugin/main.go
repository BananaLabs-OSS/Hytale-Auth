// Hytale-Auth — Pulp plugin, built on Fiber.
//
// Rewrite of the standalone Go service as a WASM plugin. All I/O goes
// through Pulp capabilities via Fiber's helpers — no raw WASM glue.
//
// Routes:
//
//	GET /tokens — refreshes OAuth and creates a fresh Hytale session,
//	              returns identity + session tokens as JSON.
//	GET /health — returns "ok".
//	GET /       — reports readiness or, during bootstrap, the
//	              device-code verification URL + user code.
//
// Storage (via storage.fs, scoped to this plugin's data dir):
//
//	refresh_token.txt — rotated OAuth refresh token.
//	profile_uuid.txt  — Hytale profile UUID for session creation.
//
// Bootstrap: when no refresh token exists, the plugin starts the OAuth
// device-code flow on init and polls the token endpoint on every step
// (gated by wall-time). Once the user authorizes, tokens persist and
// normal service begins.
//
// Build:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o hytale-auth.wasm .
package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/BananaLabs-OSS/Fiber/pulp"
	pulpgin "github.com/BananaLabs-OSS/Fiber/pulp/gin"
)

func main() {}

var (
	refreshToken string
	profileUUID  string

	setupMode       bool
	deviceCode      string
	userCode        string
	verificationURI string
	pollIntervalSec uint64 = 5
	lastPollNanos   uint64
)

func init() {
	pulp.OnInit(bootstrap)

	r := pulpgin.New()
	r.GET("/health", health)
	r.GET("/tokens", tokens)
	r.GET("/", status)

	// Declare routes but compose our own OnStep so polling runs
	// alongside HTTP dispatch.
	if err := r.RegisterRoutes(); err != nil {
		fmt.Printf("[hytale-auth] route register failed: %v\n", err)
		return
	}
	pulp.OnStep(func(ev pulp.StepEvent) error {
		if setupMode {
			pollDeviceIfDue(ev.WallTime)
		}
		return r.Dispatch(ev)
	})
}

// bootstrap runs during pulp_init. Loads any persisted credentials and
// starts the device-code flow if missing.
func bootstrap(_ []byte) error {
	if data, err := pulp.FS.Read("refresh_token.txt"); err == nil {
		refreshToken = strings.TrimSpace(string(data))
	}
	if data, err := pulp.FS.Read("profile_uuid.txt"); err == nil {
		cleaned := data
		if len(cleaned) >= 3 && cleaned[0] == 0xef && cleaned[1] == 0xbb && cleaned[2] == 0xbf {
			cleaned = cleaned[3:]
		}
		profileUUID = strings.TrimSpace(string(cleaned))
	}

	if refreshToken == "" {
		if err := startDeviceFlow(); err != nil {
			return fmt.Errorf("start device flow: %w", err)
		}
		fmt.Printf("[hytale-auth] authorize at: %s (code: %s)\n", verificationURI, userCode)
	}
	return nil
}

// ---- HTTP handlers (Gin-style) -----------------------------------------

func health(c *pulpgin.Context) {
	c.String(200, "ok")
}

func status(c *pulpgin.Context) {
	if setupMode {
		c.String(200, "Status: Needs Authorization\nURL: %s\nCode: %s\n", verificationURI, userCode)
		return
	}
	if refreshToken == "" || profileUUID == "" {
		c.String(503, "Status: Not Ready (missing credentials)")
		return
	}
	c.String(200, "Status: Ready\n")
}

func tokens(c *pulpgin.Context) {
	if setupMode {
		c.String(503, "Not authorized yet. Visit / for setup.")
		return
	}
	if refreshToken == "" || profileUUID == "" {
		c.String(503, "not authorized — missing refresh_token.txt or profile_uuid.txt")
		return
	}

	accessToken, newRefresh, err := oauthRefresh(refreshToken)
	if err != nil {
		c.String(500, "oauth refresh: %v", err)
		return
	}
	if newRefresh != "" && newRefresh != refreshToken {
		refreshToken = newRefresh
		_ = pulp.FS.Write("refresh_token.txt", []byte(newRefresh))
	}

	sessionToken, identityToken, err := createSession(accessToken, profileUUID)
	if err != nil {
		c.String(500, "session create: %v", err)
		return
	}

	c.JSON(200, pulpgin.H{
		"env": map[string]string{
			"HYTALE_SERVER_SESSION_TOKEN":  sessionToken,
			"HYTALE_SERVER_IDENTITY_TOKEN": identityToken,
		},
	})
}

// ---- OAuth flow --------------------------------------------------------

func startDeviceFlow() error {
	form := url.Values{
		"client_id": {"hytale-server"},
		"scope":     {"openid offline auth:server"},
	}
	resp, err := pulp.HTTP.Fetch(pulp.HTTPFetchRequest{
		Method:  "POST",
		URL:     "https://oauth.accounts.hytale.com/oauth2/device/auth",
		Headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
		Body:    []byte(form.Encode()),
	})
	if err != nil {
		return fmt.Errorf("device auth: %w", err)
	}
	if resp.Status != 200 {
		return fmt.Errorf("device auth status %d: %s", resp.Status, resp.Body)
	}
	var parsed struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri_complete"`
		Interval        int    `json:"interval"`
	}
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		return fmt.Errorf("decode device auth: %w", err)
	}
	deviceCode = parsed.DeviceCode
	userCode = parsed.UserCode
	verificationURI = parsed.VerificationURI
	if parsed.Interval >= 5 {
		pollIntervalSec = uint64(parsed.Interval)
	}
	setupMode = true
	return nil
}

func pollDeviceIfDue(wallNanos uint64) {
	if lastPollNanos != 0 && wallNanos-lastPollNanos < pollIntervalSec*1_000_000_000 {
		return
	}
	lastPollNanos = wallNanos

	form := url.Values{
		"client_id":   {"hytale-server"},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {deviceCode},
	}
	resp, err := pulp.HTTP.Fetch(pulp.HTTPFetchRequest{
		Method:  "POST",
		URL:     "https://oauth.accounts.hytale.com/oauth2/token",
		Headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
		Body:    []byte(form.Encode()),
	})
	if err != nil {
		return
	}

	var parsed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Error        string `json:"error"`
	}
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		return
	}
	if parsed.Error == "authorization_pending" || parsed.Error == "slow_down" {
		return
	}
	if parsed.Error != "" {
		fmt.Printf("[hytale-auth] oauth error: %s\n", parsed.Error)
		return
	}
	if parsed.RefreshToken == "" {
		return
	}

	refreshToken = parsed.RefreshToken
	_ = pulp.FS.Write("refresh_token.txt", []byte(parsed.RefreshToken))

	if uuid, err := fetchProfileUUID(parsed.AccessToken); err == nil && uuid != "" {
		profileUUID = uuid
		_ = pulp.FS.Write("profile_uuid.txt", []byte(uuid))
	}
	setupMode = false
	deviceCode = ""
	userCode = ""
	verificationURI = ""
	fmt.Printf("[hytale-auth] authorized, tokens saved\n")
}

func oauthRefresh(token string) (access, newRefresh string, err error) {
	form := url.Values{
		"client_id":     {"hytale-server"},
		"grant_type":    {"refresh_token"},
		"refresh_token": {token},
	}
	resp, err := pulp.HTTP.Fetch(pulp.HTTPFetchRequest{
		Method:  "POST",
		URL:     "https://oauth.accounts.hytale.com/oauth2/token",
		Headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
		Body:    []byte(form.Encode()),
	})
	if err != nil {
		return "", "", err
	}
	if resp.Status != 200 {
		return "", "", fmt.Errorf("oauth status %d: %s", resp.Status, resp.Body)
	}
	var parsed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		return "", "", fmt.Errorf("parse oauth body: %w", err)
	}
	return parsed.AccessToken, parsed.RefreshToken, nil
}

func createSession(accessToken, uuid string) (sessionToken, identityToken string, err error) {
	body := fmt.Sprintf(`{"uuid": %q}`, uuid)
	resp, err := pulp.HTTP.Fetch(pulp.HTTPFetchRequest{
		Method: "POST",
		URL:    "https://sessions.hytale.com/game-session/new",
		Headers: map[string]string{
			"Authorization": "Bearer " + accessToken,
			"Content-Type":  "application/json",
		},
		Body: []byte(body),
	})
	if err != nil {
		return "", "", err
	}
	if resp.Status != 200 {
		return "", "", fmt.Errorf("session status %d: %s", resp.Status, resp.Body)
	}
	var parsed struct {
		SessionToken  string `json:"sessionToken"`
		IdentityToken string `json:"identityToken"`
	}
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		return "", "", fmt.Errorf("parse session body: %w", err)
	}
	return parsed.SessionToken, parsed.IdentityToken, nil
}

func fetchProfileUUID(accessToken string) (string, error) {
	resp, err := pulp.HTTP.Fetch(pulp.HTTPFetchRequest{
		Method: "GET",
		URL:    "https://account-data.hytale.com/my-account/get-profiles",
		Headers: map[string]string{
			"Authorization": "Bearer " + accessToken,
		},
	})
	if err != nil {
		return "", err
	}
	if resp.Status != 200 {
		return "", fmt.Errorf("get-profiles status %d: %s", resp.Status, resp.Body)
	}
	var parsed struct {
		Profiles []struct {
			UUID string `json:"uuid"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		return "", fmt.Errorf("decode profiles: %w", err)
	}
	if len(parsed.Profiles) == 0 {
		return "", fmt.Errorf("no profiles in response")
	}
	return parsed.Profiles[0].UUID, nil
}
