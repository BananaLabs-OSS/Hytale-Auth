// Hytale-Auth — Pulp plugin port.
//
// Replaces the standalone Go service with a WASM plugin that speaks the
// Pulp HTTP and storage capabilities. Identical external behavior:
//
//	GET /tokens — refreshes OAuth and creates a new Hytale session,
//	              returns identity + session tokens as JSON.
//	GET /health — returns "ok".
//	GET /       — reports readiness / authorization status.
//
// Storage:
//
//	refresh_token.txt — rotated OAuth refresh token, read at boot,
//	                    rewritten on every successful refresh.
//	profile_uuid.txt  — Hytale profile UUID used in session creation.
//
// Device-code bootstrap (operator-facing setup flow) is not ported in
// this first cut; provision the two files before running the plugin.
//
// Build:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o hytale-auth.wasm .
package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unsafe"

	"github.com/vmihailenco/msgpack/v5"
)

func main() {}

//go:wasmimport pulp http_register
func hostHTTPRegister(ptr, ln uint32) uint32

//go:wasmimport pulp http_respond
func hostHTTPRespond(ptr, ln uint32) uint32

//go:wasmimport pulp http_fetch
func hostHTTPFetch(reqPtr, reqLen, respPtrOut, respLenOut uint32) uint32

//go:wasmimport pulp fs_read
func hostFSRead(pathPtr, pathLen, dataPtrOut, dataLenOut uint32) uint32

//go:wasmimport pulp fs_write
func hostFSWrite(pathPtr, pathLen, dataPtr, dataLen uint32) uint32

type stepEvent struct {
	Kind    string             `msgpack:"kind"`
	Payload msgpack.RawMessage `msgpack:"payload"`
}

type httpRequest struct {
	ID      uint64            `msgpack:"id"`
	Method  string            `msgpack:"method"`
	Path    string            `msgpack:"path"`
	Params  map[string]string `msgpack:"params"`
	Query   map[string]string `msgpack:"query"`
	Headers map[string]string `msgpack:"headers"`
	Body    []byte            `msgpack:"body"`
}

type httpResponse struct {
	ID      uint64            `msgpack:"id"`
	Status  uint32            `msgpack:"status"`
	Headers map[string]string `msgpack:"headers"`
	Body    []byte            `msgpack:"body"`
}

type httpFetchRequest struct {
	Method  string            `msgpack:"method"`
	URL     string            `msgpack:"url"`
	Headers map[string]string `msgpack:"headers"`
	Body    []byte            `msgpack:"body"`
}

type httpFetchResponse struct {
	Status  uint32            `msgpack:"status"`
	Headers map[string]string `msgpack:"headers"`
	Body    []byte            `msgpack:"body"`
}

var (
	refreshToken string
	profileUUID  string

	pinned [][]byte
)

//go:wasmexport pulp_alloc
func pulpAlloc(size uint32) uint32 {
	if size == 0 {
		return 0
	}
	buf := make([]byte, size)
	pinned = append(pinned, buf)
	return uint32(uintptr(unsafe.Pointer(&buf[0])))
}

//go:wasmexport pulp_free
func pulpFree(ptr, size uint32) {
	_ = ptr
	_ = size
}

//go:wasmexport pulp_init
func pulpInit(cfgPtr, cfgLen uint32) int32 {
	_ = cfgPtr
	_ = cfgLen

	if data, ok := fsRead("refresh_token.txt"); ok {
		refreshToken = strings.TrimSpace(string(data))
	}
	if data, ok := fsRead("profile_uuid.txt"); ok {
		cleaned := data
		if len(cleaned) >= 3 && cleaned[0] == 0xef && cleaned[1] == 0xbb && cleaned[2] == 0xbf {
			cleaned = cleaned[3:]
		}
		profileUUID = strings.TrimSpace(string(cleaned))
	}

	for _, r := range [][2]string{
		{"GET", "/tokens"},
		{"GET", "/health"},
		{"GET", "/"},
	} {
		if code := registerRoute(r[0], r[1]); code != 0 {
			return int32(100 + code)
		}
	}
	return 0
}

//go:wasmexport pulp_step
func pulpStep(inputPtr, inputLen uint32) int32 {
	if inputLen < 20 {
		return 1
	}
	raw := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(inputPtr))), inputLen)
	payloadLen := binary.LittleEndian.Uint32(raw[16:20])
	if payloadLen == 0 {
		return 0
	}
	payload := raw[20 : 20+payloadLen]

	var ev stepEvent
	if err := msgpack.Unmarshal(payload, &ev); err != nil {
		return 2
	}
	if ev.Kind != "http.request" {
		return 0
	}

	var req httpRequest
	if err := msgpack.Unmarshal(ev.Payload, &req); err != nil {
		return 3
	}
	resp := route(req)
	return respond(resp)
}

//go:wasmexport pulp_shutdown
func pulpShutdown() int32 {
	pinned = nil
	return 0
}

func route(req httpRequest) httpResponse {
	switch req.Path {
	case "/health":
		return textResponse(req.ID, 200, "ok")
	case "/":
		if refreshToken == "" || profileUUID == "" {
			return textResponse(req.ID, 503, "Status: Needs Bootstrap (refresh_token.txt + profile_uuid.txt)")
		}
		return textResponse(req.ID, 200, "Status: Ready")
	case "/tokens":
		return handleTokens(req)
	default:
		return textResponse(req.ID, 404, "not found")
	}
}

func handleTokens(req httpRequest) httpResponse {
	if refreshToken == "" || profileUUID == "" {
		return textResponse(req.ID, 503, "not authorized — missing refresh_token.txt or profile_uuid.txt")
	}

	accessToken, newRefresh, err := oauthRefresh(refreshToken)
	if err != nil {
		return textResponse(req.ID, 500, "oauth refresh: "+err.Error())
	}
	if newRefresh != "" && newRefresh != refreshToken {
		refreshToken = newRefresh
		_ = fsWrite("refresh_token.txt", []byte(newRefresh))
	}

	sessionToken, identityToken, err := createSession(accessToken, profileUUID)
	if err != nil {
		return textResponse(req.ID, 500, "session create: "+err.Error())
	}

	body, err := json.Marshal(map[string]any{
		"env": map[string]string{
			"HYTALE_SERVER_SESSION_TOKEN":  sessionToken,
			"HYTALE_SERVER_IDENTITY_TOKEN": identityToken,
		},
	})
	if err != nil {
		return textResponse(req.ID, 500, "marshal response: "+err.Error())
	}
	return httpResponse{
		ID:      req.ID,
		Status:  200,
		Headers: map[string]string{"Content-Type": "application/json"},
		Body:    body,
	}
}

func oauthRefresh(token string) (access, newRefresh string, err error) {
	form := url.Values{
		"client_id":     {"hytale-server"},
		"grant_type":    {"refresh_token"},
		"refresh_token": {token},
	}
	resp, err := fetch(httpFetchRequest{
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
	resp, err := fetch(httpFetchRequest{
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

func fetch(req httpFetchRequest) (httpFetchResponse, error) {
	reqBytes, err := msgpack.Marshal(req)
	if err != nil {
		return httpFetchResponse{}, fmt.Errorf("marshal fetch: %w", err)
	}
	pinned = append(pinned, reqBytes)

	var respPtr, respLen uint32
	code := hostHTTPFetch(
		uint32(uintptr(unsafe.Pointer(&reqBytes[0]))),
		uint32(len(reqBytes)),
		uint32(uintptr(unsafe.Pointer(&respPtr))),
		uint32(uintptr(unsafe.Pointer(&respLen))),
	)
	if code != 0 {
		return httpFetchResponse{}, fmt.Errorf("http_fetch host code %d", code)
	}
	if respLen == 0 {
		return httpFetchResponse{}, fmt.Errorf("http_fetch returned empty body")
	}
	respBytes := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(respPtr))), respLen)
	var resp httpFetchResponse
	if err := msgpack.Unmarshal(respBytes, &resp); err != nil {
		return httpFetchResponse{}, fmt.Errorf("decode fetch resp: %w", err)
	}
	return resp, nil
}

func registerRoute(method, path string) uint32 {
	reg := struct {
		Method string `msgpack:"method"`
		Path   string `msgpack:"path"`
	}{Method: method, Path: path}
	data, err := msgpack.Marshal(reg)
	if err != nil || len(data) == 0 {
		return 99
	}
	pinned = append(pinned, data)
	return hostHTTPRegister(uint32(uintptr(unsafe.Pointer(&data[0]))), uint32(len(data)))
}

func respond(resp httpResponse) int32 {
	data, err := msgpack.Marshal(resp)
	if err != nil {
		return 4
	}
	pinned = append(pinned, data)
	if code := hostHTTPRespond(uint32(uintptr(unsafe.Pointer(&data[0]))), uint32(len(data))); code != 0 {
		return int32(300 + code)
	}
	return 0
}

func textResponse(id uint64, status uint32, body string) httpResponse {
	return httpResponse{
		ID:      id,
		Status:  status,
		Headers: map[string]string{"Content-Type": "text/plain; charset=utf-8"},
		Body:    []byte(body),
	}
}

func fsRead(path string) ([]byte, bool) {
	pathBytes := []byte(path)
	pinned = append(pinned, pathBytes)
	var dataPtr, dataLen uint32
	code := hostFSRead(
		uint32(uintptr(unsafe.Pointer(&pathBytes[0]))),
		uint32(len(pathBytes)),
		uint32(uintptr(unsafe.Pointer(&dataPtr))),
		uint32(uintptr(unsafe.Pointer(&dataLen))),
	)
	if code != 0 {
		return nil, false
	}
	if dataLen == 0 {
		return nil, true
	}
	data := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(dataPtr))), dataLen)
	out := make([]byte, dataLen)
	copy(out, data)
	return out, true
}

func fsWrite(path string, data []byte) bool {
	pathBytes := []byte(path)
	pinned = append(pinned, pathBytes, data)
	var dataPtr uint32
	var dataLen uint32
	if len(data) > 0 {
		dataPtr = uint32(uintptr(unsafe.Pointer(&data[0])))
		dataLen = uint32(len(data))
	}
	code := hostFSWrite(
		uint32(uintptr(unsafe.Pointer(&pathBytes[0]))),
		uint32(len(pathBytes)),
		dataPtr,
		dataLen,
	)
	return code == 0
}
