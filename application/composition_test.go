package application

import (
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestHytaleAuthUsesGenericEnginesAndLuaComposition(t *testing.T) {
	manifest, err := os.ReadFile("pulp.app.toml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(manifest)
	for _, required := range []string{
		`schema_version = 1`, `name = "hytale-auth"`, `"../api-cell/pulp.cell.toml"`,
		`"../../pulp-engines/http-json-cell/pulp.cell.toml"`,
		`"../../pulp-engines/scoped-kv-fs-cell/pulp.cell.toml"`,
		`manifest = "lua-orchestrator.cell.toml"`, `script = "hytale-auth.lua"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("application manifest is missing %q", required)
		}
	}
	if strings.Contains(text, `../pulp-cell/pulp.cell.toml`) {
		t.Fatal("application still composes the legacy product engine")
	}
	script, err := os.ReadFile("hytale-auth.lua")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		`pulp.on(event, handler)`, `engine.http-json.v1.request`, `storage.kv-fs.v1.get`,
		`hytale-auth.http.tokens.v1`, `hytale-auth.tick.v1`,
	} {
		if !strings.Contains(string(script), required) {
			t.Fatalf("Lua composition is missing %q", required)
		}
	}
	want := fmt.Sprintf("sha256 = \"%x\"", sha256.Sum256(script))
	if !strings.Contains(text, want) {
		t.Fatalf("orchestrator digest is missing or stale; want %s", want)
	}
}

func TestAdaptersOwnOnlyRequiredCapabilities(t *testing.T) {
	api, err := os.ReadFile("../api-cell/pulp.cell.toml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(api), `capabilities = ["transport.http.inbound"]`) ||
		strings.Contains(string(api), "transport.http.outbound") || strings.Contains(string(api), "storage.fs") {
		t.Fatal("API adapter capability boundary is too broad")
	}
	lua, err := os.ReadFile("lua-orchestrator.cell.toml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(lua), `capabilities = []`) {
		t.Fatal("Lua orchestrator must have no host capabilities")
	}
}
