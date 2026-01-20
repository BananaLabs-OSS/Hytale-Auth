package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

var refreshToken string
var profileUUID string

func main() {
	refreshToken = loadToken()
	if refreshToken == "" {
		fmt.Println("ERROR: No refresh token found")
		return
	}
	fmt.Println("Token loaded, length:", len(refreshToken))

	profileUUID = os.Getenv("HYTALE_PROFILE_UUID")
	if profileUUID == "" {
		fmt.Println("ERROR: No profile UUID found")
		return
	}

	http.HandleFunc("/tokens", tokensHandler)
	http.HandleFunc("/health", healthHandler)
	http.ListenAndServe(":3001", nil)
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
	// Get access token
	accessToken, newRefresh, err := refreshAccessToken()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

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
