package main

import (
	"path/filepath"
	"testing"

	"github.com/BananaLabs-OSS/Pulp/run"
)

func TestHytaleAuthApplicationManifestLoadsWithoutStartingAuth(t *testing.T) {
	app, err := run.NewDirectApplicationRuntime(filepath.Join("..", "application", "pulp.app.toml"), run.DirectApplicationOptions{
		InstanceID: "manifest-test",
	})
	if err != nil {
		t.Fatalf("load Hytale Auth application manifest: %v", err)
	}
	identity := app.Identity()
	if identity.ApplicationID != "hytale-auth" || identity.InstanceID != "manifest-test" {
		t.Fatalf("application identity = %#v", identity)
	}
}
