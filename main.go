package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	refreshToken string
	profileUUID  string
	setupMode    bool
	deviceURL    string
	deviceCode   string
	userCode     string
)

var config Config

type Config struct {
	Port    string `json:"port"`
	DataDir string `json:"data_dir"`
}

func main() {
	config = loadConfig()
	fmt.Printf("Starting on port %s, data dir: %s\n", config.Port, config.DataDir)

	profileUUID = loadUUID()
	refreshToken = loadToken()
	if refreshToken != "" {
		fmt.Println("Verifying stored token...")
		_, newRefresh, err := refreshAccessToken()
		if err == nil && newRefresh != "" {
			saveToken(newRefresh)
			fmt.Println("  Token valid")
		} else {
			fmt.Println(" ️  Token invalid:", err)
			startDeviceFlow()
		}
	} else {
		startDeviceFlow()
	}

	http.HandleFunc("/tokens", tokensHandler)
	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/", setupHandler)
	http.ListenAndServe(":"+config.Port, nil)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(200)
	w.Write([]byte("ok"))
}

func loadToken() string {
	path := filepath.Join(config.DataDir, "refresh_token.txt")
	if data, err := os.ReadFile(path); err == nil {
		token := strings.TrimSpace(string(data))
		if token != "" {
			return token
		}
	}
	return os.Getenv("HYTALE_REFRESH_TOKEN")
}

func loadUUID() string {
	path := filepath.Join(config.DataDir, "profile_uuid.txt")
	if data, err := os.ReadFile(path); err == nil {
		if len(data) >= 3 && data[0] == 0xef && data[1] == 0xbb && data[2] == 0xbf {
			data = data[3:]
		}
		uuid := strings.TrimSpace(string(data))
		if uuid != "" {
			return uuid
		}
	}
	return os.Getenv("HYTALE_PROFILE_UUID")
}

func saveUUID(uuid string) {
	path := filepath.Join(config.DataDir, "profile_uuid.txt")
	os.WriteFile(path, []byte(uuid), 0600)
}

func refreshAccessToken() (accessToken string, newRefreshToken string, err error) {
	fmt.Println("Calling OAuth...")

	resp, err := http.PostForm("https://oauth.accounts.hytale.com/oauth2/token", url.Values{
		"client_id":     {"hytale-server"},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	})
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	fmt.Println("OAuth status:", resp.StatusCode)

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("oauth returned %d: %s", resp.StatusCode, string(respBody))
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("failed to read oauth response: %w", err)
	}
	fmt.Println("OAuth response received, parsing tokens...")

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", "", fmt.Errorf("failed to parse oauth response: %w", err)
	}

	fmt.Printf("Parsed access token length: %d\n", len(result.AccessToken))

	return result.AccessToken, result.RefreshToken, nil
}

func saveToken(token string) {
	refreshToken = token
	path := filepath.Join(config.DataDir, "refresh_token.txt")
	os.WriteFile(path, []byte(token), 0600)
}

func createSession(accessToken string) (sessionToken string, identityToken string, err error) {
	if len(accessToken) >= 20 {
		fmt.Println(" Creating session with account access token: ", accessToken[:20]+"...")
	} else {
		fmt.Println(" Creating session with account access token: ", accessToken+"...")
	}

	// Build JSON body
	body := fmt.Sprintf(`{"uuid": "%s"}`, profileUUID)
	fmt.Println("Request body:", body)

	// Create request
	req, err := http.NewRequest("POST", "https://sessions.hytale.com/game-session/new", strings.NewReader(body))
	if err != nil {
		return "", "", err
	}
	// Add headers
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	// Send
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	fmt.Println("Session status:", resp.StatusCode)
	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		fmt.Println("Session error:", string(respBody))
		return "", "", fmt.Errorf("session failed: %d - %s", resp.StatusCode, string(respBody))
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("failed to read session response: %w", err)
	}

	// Parse response
	var result struct {
		SessionToken  string `json:"sessionToken"`
		IdentityToken string `json:"identityToken"`
		ExpiresAt     string `json:"expiresAt"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", "", fmt.Errorf("failed to parse session response: %w", err)
	}

	fmt.Printf("Session created, expires: %s\n", result.ExpiresAt)

	return result.SessionToken, result.IdentityToken, nil
}

func tokensHandler(w http.ResponseWriter, r *http.Request) {
	if setupMode {
		http.Error(w, "Not authorized yet. Visit / for setup.", 503)
		return
	}

	// Get access token
	accessToken, newRefresh, err := refreshAccessToken()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// Save rotated refresh token only when the endpoint returned one;
	// an empty newRefresh must not blank the stored token.
	if newRefresh != "" {
		saveToken(newRefresh)
	}

	// Create session
	sessionToken, identityToken, err := createSession(accessToken)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// Return as JSON
	json.NewEncoder(w).Encode(map[string]interface{}{
		"env": map[string]string{
			"HYTALE_SERVER_SESSION_TOKEN":  sessionToken,
			"HYTALE_SERVER_IDENTITY_TOKEN": identityToken,
		},
	})
}

func setupHandler(w http.ResponseWriter, r *http.Request) {
	if setupMode {
		fmt.Fprintf(w, "Status: Needs Authorization\nURL: %s\nCode: %s\n", deviceURL, userCode)
		return
	}
	fmt.Fprintf(w, "Status: Ready\n")
}

func startDeviceFlow() {
	resp, err := http.PostForm("https://oauth.accounts.hytale.com/oauth2/device/auth",
		url.Values{
			"client_id": {"hytale-server"},
			"scope":     {"openid offline auth:server"},
		})
	if err != nil {
		fmt.Println("Device flow error:", err)
		return
	}
	defer resp.Body.Close()

	var result struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri_complete"`
		Interval        int    `json:"interval"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		fmt.Println("Device flow decode error:", err)
		return
	}

	deviceCode = result.DeviceCode
	userCode = result.UserCode
	deviceURL = result.VerificationURI
	setupMode = true

	fmt.Println("Authorization required")
	fmt.Printf("   Go to: %s\n", deviceURL)
	fmt.Printf("   Code:  %s\n", userCode)

	go pollForToken(result.Interval)
}

func pollForToken(interval int) {
	if interval < 5 {
		interval = 5
	}

	for setupMode {
		time.Sleep(time.Duration(interval) * time.Second)

		resp, err := http.PostForm("https://oauth.accounts.hytale.com/oauth2/token",
			url.Values{
				"client_id":   {"hytale-server"},
				"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
				"device_code": {deviceCode},
			})
		if err != nil {
			continue
		}

		var result struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			Error        string `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			continue
		}
		resp.Body.Close()

		if result.Error == "authorization_pending" {
			continue
		}
		// RFC 8628 §3.5: expired_token and access_denied are terminal —
		// the device code window has closed or the user denied. Stop polling.
		if result.Error == "expired_token" || result.Error == "access_denied" {
			fmt.Println("Auth failed (terminal):", result.Error)
			setupMode = false
			return
		}
		if result.Error != "" {
			fmt.Println("Auth error:", result.Error)
			continue
		}

		if result.RefreshToken != "" {
			saveToken(result.RefreshToken)
			refreshToken = result.RefreshToken

			// Fetch profile UUID from API
			if result.AccessToken != "" {
				uuid, err := fetchProfileUUID(result.AccessToken)
				if err == nil {
					profileUUID = uuid
					saveUUID(profileUUID)
					fmt.Println("Profile UUID:", profileUUID)
				} else {
					fmt.Println("Failed to fetch profile:", err)
				}
			}

			setupMode = false
			fmt.Println("Authorized!")
		}
	}
}

func fetchProfileUUID(accessToken string) (string, error) {
	req, _ := http.NewRequest("GET", "https://account-data.hytale.com/my-account/get-profiles", nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		Profiles []struct {
			UUID string `json:"uuid"`
		} `json:"profiles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to decode profiles: %w", err)
	}

	if len(result.Profiles) > 0 {
		return result.Profiles[0].UUID, nil
	}
	return "", fmt.Errorf("no profiles found")
}

func loadConfig() Config {
	cfg := Config{
		Port:    "8081",
		DataDir: ".",
	}

	// Load from file if exists
	if data, err := os.ReadFile("config.json"); err == nil {
		json.Unmarshal(data, &cfg)
	}

	// Env vars override
	if p := os.Getenv("PORT"); p != "" {
		cfg.Port = p
	}
	if d := os.Getenv("DATA_DIR"); d != "" {
		cfg.DataDir = d
	}

	// CLI flags override everything
	port := flag.String("port", "", "HTTP port")
	dataDir := flag.String("data-dir", "", "Directory for data files")
	flag.Parse()

	if *port != "" {
		cfg.Port = *port
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}

	return cfg
}
