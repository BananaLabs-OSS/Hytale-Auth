package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var refreshToken string
var profileUUID string
var setupMode bool
var deviceURL string
var deviceCode string
var userCode string

func main() {
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
	http.ListenAndServe(":3002", nil)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(200)
	w.Write([]byte("ok"))
}

func loadToken() string {
	if data, err := os.ReadFile("refresh_token.txt"); err == nil {
		token := strings.TrimSpace(string(data))
		if token != "" {
			return token
		}
	}
	return os.Getenv("HYTALE_REFRESH_TOKEN")
}

func loadUUID() string {
	if data, err := os.ReadFile("profile_uuid.txt"); err == nil {
		// Strip BOM if present
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
	os.WriteFile("profile_uuid.txt", []byte(uuid), 0600)
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

	respBody, _ := io.ReadAll(resp.Body)
	fmt.Println("OAuth response received, parsing tokens...")

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	json.Unmarshal(respBody, &result)

	fmt.Printf("Parsed access token length: %d\n", len(result.AccessToken))

	return result.AccessToken, result.RefreshToken, nil
}

func saveToken(token string) {
	refreshToken = token
	os.WriteFile("refresh_token.txt", []byte(token), 0600)
}

func createSession(accessToken string) (sessionToken string, identityToken string, err error) {
	fmt.Println(" Creating session with account access token: ", accessToken[:20]+"...")

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

	respBody, _ := io.ReadAll(resp.Body)

	// Parse response
	var result struct {
		SessionToken  string `json:"sessionToken"`
		IdentityToken string `json:"identityToken"`
		ExpiresAt     string `json:"expiresAt"`
	}
	json.Unmarshal(respBody, &result)

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

	fmt.Println("Full access token:", accessToken)

	// Save rotated refresh token
	saveToken(newRefresh)

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
	json.NewDecoder(resp.Body).Decode(&result)

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
		json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()

		if result.Error == "authorization_pending" {
			continue
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
	json.NewDecoder(resp.Body).Decode(&result)

	if len(result.Profiles) > 0 {
		return result.Profiles[0].UUID, nil
	}
	return "", fmt.Errorf("no profiles found")
}
