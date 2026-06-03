// Hytale-Auth — Pulp cell, built on Fiber.
//
// Rewrite of the standalone Go service as a WASM cell. All I/O goes
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
// Storage (via storage.fs, scoped to this cell's data dir):
//
//	refresh_token.txt — rotated OAuth refresh token.
//	profile_uuid.txt  — Hytale profile UUID for session creation.
//
// Bootstrap: when no refresh token exists, the cell starts the OAuth
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
	"log"
	"net/url"
	"os"
	"strings"

	"github.com/BananaLabs-OSS/Fiber/pulp"
	pulpgin "github.com/BananaLabs-OSS/Fiber/pulp/gin"
	"github.com/BananaLabs-OSS/Fiber/pulp/gin/middleware"
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

	// serviceToken gates the credential-issuing route. Read from the
	// SERVICE_TOKEN env (set by the Pulp host) so the secret stays out of
	// the committed pulp.cell.toml.
	serviceToken string
)

func init() {
	pulp.OnInit(bootstrap)

	// Auth posture: auth-available-not-mandatory (matches Peel pulp-cell).
	// GET /tokens mints LIVE Hytale server session + identity tokens — a
	// credential-issuing endpoint — so it is gated on the X-Service-Token
	// shared secret (constant-time, via middleware.ServiceAuth), the same
	// SERVICE_TOKEN pattern the other cells use. Gating is enforced ONLY
	// when SERVICE_TOKEN is non-empty: an empty token leaves /tokens
	// unauthenticated so the existing Bananagine pre-start hook (which sends
	// no header today) keeps working — no 401, no outage. Deliberately NOT
	// fail-closed: an empty token must not block startup or token issuance.
	// To ENABLE auth: set SERVICE_TOKEN here AND have Bananagine send the
	// same X-Service-Token, in lockstep.
	serviceToken = os.Getenv("SERVICE_TOKEN")

	r := pulpgin.New()
	r.GET("/health", health)

	// /tokens rides a root group so the path stays "/tokens" (byte-parity
	// with native); only the auth middleware is interposed when a token is
	// configured.
	var minting *pulpgin.RouterGroup
	if serviceToken != "" {
		minting = r.Group("", middleware.ServiceAuth(serviceToken))
		log.Printf("Token-mint auth ENABLED (X-Service-Token required on /tokens)")
	} else {
		minting = r.Group("")
		log.Printf("Token-mint auth OFF (SERVICE_TOKEN empty); to enable, set SERVICE_TOKEN here AND have the Bananagine pre-start hook send X-Service-Token")
	}
	minting.GET("/tokens", tokens)

	r.GET("/", status)

	// Declare routes but compose our own OnStep so polling runs
	// alongside HTTP dispatch. Note: Pulp's OnStep only fires on
	// payload-carrying events (HTTP/WS). There is no idle tick, so
	// device-code polling advances only when HTTP traffic arrives.
	// This is the established pattern across all Pulp cells (matches
	// Bananasplit matcher.TickIfDue). In practice the operator will
	// hit / to see the code, which is enough to drive the poll.
	if err := r.RegisterRoutes(); err != nil {
		// Cell-only — native Hytale-Auth has no equivalent (routes are mux.HandleFunc).
		log.Printf("route register failed: %v", err)
		return
	}
	pulp.OnStep(func(ev pulp.StepEvent) error {
		if setupMode {
			// Seed lastPollNanos on first observation so the first poll
			// waits a full interval after device flow start — matches
			// native pollForToken's `time.Sleep(interval)` BEFORE the
			// first request, not before subsequent ones.
			if lastPollNanos == 0 {
				lastPollNanos = ev.WallTime
			} else {
				pollDeviceIfDue(ev.WallTime)
			}
		}
		return r.Dispatch(ev)
	})
}

// bootstrap runs during pulp_init. Loads any persisted credentials and
// starts the device-code flow if missing.
//
// Credential resolution ladder (matches native main.go):
//  1. File in the cell's scoped storage (refresh_token.txt, profile_uuid.txt)
//  2. Host-forwarded env var (HYTALE_REFRESH_TOKEN, HYTALE_PROFILE_UUID)
//  3. Device-code flow (interactive authorization)
//
// The env-var leg only works if the Pulp host forwards those keys into
// WASI's env list. The host currently forwards a narrow allowlist; if
// the keys aren't forwarded, os.Getenv returns "" and we fall through
// to the device flow — same net behavior as native when env is unset.
func bootstrap(_ []byte) error {
	if data, err := pulp.FS.Read("refresh_token.txt"); err == nil {
		refreshToken = strings.TrimSpace(string(data))
	}
	if refreshToken == "" {
		refreshToken = os.Getenv("HYTALE_REFRESH_TOKEN")
	}

	if data, err := pulp.FS.Read("profile_uuid.txt"); err == nil {
		cleaned := data
		if len(cleaned) >= 3 && cleaned[0] == 0xef && cleaned[1] == 0xbb && cleaned[2] == 0xbf {
			cleaned = cleaned[3:]
		}
		profileUUID = strings.TrimSpace(string(cleaned))
	}
	if profileUUID == "" {
		profileUUID = os.Getenv("HYTALE_PROFILE_UUID")
	}

	if refreshToken != "" {
		// Verify stored token still works. If the refresh call succeeds
		// and hands back a rotated token, persist it; on failure, fall
		// through to the device flow so the operator can re-authorize.
		// Matches native main.go's "Verifying stored token..." block.
		fmt.Println("Verifying stored token...")
		if _, newRefresh, err := oauthRefresh(refreshToken); err == nil {
			if newRefresh != "" && newRefresh != refreshToken {
				refreshToken = newRefresh
				_ = pulp.FS.Write("refresh_token.txt", []byte(newRefresh))
			}
			fmt.Println("  Token valid")
			return nil
		} else {
			// Parity with native Hytale-Auth/main.go:45 — keeps the U+FE0F variation selector + double space.
			fmt.Println(" ️  Token invalid:", err)
			refreshToken = ""
		}
	}

	if refreshToken == "" {
		if err := startDeviceFlow(); err != nil {
			return fmt.Errorf("start device flow: %w", err)
		}
		fmt.Println("Authorization required")
		fmt.Printf("   Go to: %s\n", verificationURI)
		fmt.Printf("   Code:  %s\n", userCode)
	}
	return nil
}

// ---- HTTP handlers (Gin-style) -----------------------------------------

func health(c *pulpgin.Context) {
	c.String(200, "ok")
}

func status(c *pulpgin.Context) {
	if setupMode {
		// The device-flow user_code is a short-lived shared secret (RFC 8628)
		// meant only for the legitimate operator — it is already emitted to
		// the operator console in bootstrap. Do NOT disclose it (or the
		// pre-filled verification_uri_complete) over this unauthenticated
		// route: any caller that reaches the port during the bootstrap window
		// could otherwise read the code and complete the OAuth approval. The
		// operator reads the code/URL from the cell logs instead.
		c.String(200, "Status: Needs Authorization\nSee cell logs for the verification URL and code.\n")
		return
	}
	// Parity with native setupHandler: once setupMode flips off, native
	// reports "Status: Ready" unconditionally (no missing-credentials
	// gate). Mirror that exactly so downstream byte-for-byte compare
	// passes.
	c.String(200, "Status: Ready\n")
}

func tokens(c *pulpgin.Context) {
	// Parity with native tokensHandler: native gates only on setupMode,
	// then falls through to refreshAccessToken / createSession. No
	// separate "missing creds" 503 — if refreshToken is empty the
	// refresh call below 500s naturally.
	if setupMode {
		// Native uses http.Error which appends a trailing newline and
		// sets Content-Type: text/plain; charset=utf-8 + nosniff. Mirror
		// exactly (nosniff header included for byte-parity with net/http).
		httpErrorLike(c, 503, "Not authorized yet. Visit / for setup.")
		return
	}

	accessToken, newRefresh, err := oauthRefresh(refreshToken)
	if err != nil {
		// Native: http.Error(w, err.Error(), 500) — no label prefix.
		httpErrorLike(c, 500, err.Error())
		return
	}
	// Native saveToken is unconditional; mirror that ordering but keep
	// the non-empty guard (Hytale OAuth always rotates, so this is a
	// no-op in practice; the guard prevents wiping state on a spec
	// violation).
	if newRefresh != "" && newRefresh != refreshToken {
		refreshToken = newRefresh
		_ = pulp.FS.Write("refresh_token.txt", []byte(newRefresh))
	}

	sessionToken, identityToken, err := createSession(accessToken, profileUUID)
	if err != nil {
		httpErrorLike(c, 500, err.Error())
		return
	}

	// Native uses json.NewEncoder(w).Encode — no explicit Content-Type
	// (Go sniffs to text/plain; charset=utf-8 for JSON) and appends a
	// trailing newline. Mirror by marshaling + Data with sniffed CT +
	// appended \n so the wire bytes match native exactly.
	body, err := jsonMarshalNative(pulpgin.H{
		"env": map[string]string{
			"HYTALE_SERVER_SESSION_TOKEN":  sessionToken,
			"HYTALE_SERVER_IDENTITY_TOKEN": identityToken,
		},
	})
	if err != nil {
		httpErrorLike(c, 500, err.Error())
		return
	}
	// Content-Type: native doesn't set it before Encode, so Go's
	// ResponseWriter sniffer runs on the first Write. The sniffer
	// returns "text/plain; charset=utf-8" for JSON-starting ASCII
	// (Go's DetectContentType has no JSON rule). Match that exactly.
	c.Data(200, "text/plain; charset=utf-8", body)
}

// httpErrorLike mirrors net/http.Error's wire format: appends a
// trailing newline to msg, sets Content-Type: text/plain; charset=utf-8
// and X-Content-Type-Options: nosniff. Use wherever native calls
// http.Error so the cell's bytes match the native service byte-for-byte.
func httpErrorLike(c *pulpgin.Context, status int, msg string) {
	c.Header("X-Content-Type-Options", "nosniff")
	c.Data(status, "text/plain; charset=utf-8", []byte(msg+"\n"))
}

// jsonMarshalNative matches json.NewEncoder(w).Encode: marshals obj
// and appends a trailing newline. encoding/json's Encoder adds the
// newline; Marshal alone does not.
func jsonMarshalNative(obj any) ([]byte, error) {
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
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
	if lastPollNanos != 0 {
		delta := int64(wallNanos) - int64(lastPollNanos)
		if delta < 0 {
			return // clock skew — wait for next tick
		}
		if delta < int64(pollIntervalSec)*1_000_000_000 {
			return
		}
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
		// Native pollForToken logs "Auth error:" prefix — mirror.
		fmt.Println("Auth error:", parsed.Error)
		return
	}
	if parsed.RefreshToken == "" {
		return
	}

	refreshToken = parsed.RefreshToken
	_ = pulp.FS.Write("refresh_token.txt", []byte(parsed.RefreshToken))

	// Fetch profile UUID from API. Native logs "Profile UUID:" on
	// success, "Failed to fetch profile:" on error.
	if parsed.AccessToken != "" {
		if uuid, err := fetchProfileUUID(parsed.AccessToken); err == nil {
			profileUUID = uuid
			_ = pulp.FS.Write("profile_uuid.txt", []byte(uuid))
			fmt.Println("Profile UUID:", profileUUID)
		} else {
			fmt.Println("Failed to fetch profile:", err)
		}
	}
	// Do NOT clear deviceCode/userCode/verificationURI — native leaves
	// them populated after success (pollForToken only flips setupMode
	// and never zeroes the strings). Status endpoint gates on setupMode
	// alone, so clearing here would be a silent divergence.
	setupMode = false
	fmt.Println("Authorized!")
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
	// Use literal "%s" not %q to match native main.go's request body
	// byte-for-byte. Native does not Go-escape the UUID; parity means
	// we don't either. UUIDs are alphanumeric+hyphen so this is safe.
	body := fmt.Sprintf(`{"uuid": "%s"}`, uuid)
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
