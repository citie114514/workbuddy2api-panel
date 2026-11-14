package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestKeepaliveRefreshWindowConfig(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
	if c.Schedule.KeepaliveRefreshWindow != "168h" || c.KeepaliveRefreshWindowDur != 168*time.Hour {
		t.Fatalf("default window=%q/%v want 168h", c.Schedule.KeepaliveRefreshWindow, c.KeepaliveRefreshWindowDur)
	}

	fp := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(fp, []byte(`{"schedule":{"keepalive_refresh_window":"24h"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if got.KeepaliveRefreshWindowDur != 24*time.Hour {
		t.Fatalf("explicit window=%v want 24h", got.KeepaliveRefreshWindowDur)
	}

	// 老配置没有该键：必须回落默认，不能报错。
	if err := os.WriteFile(fp, []byte(`{"schedule":{"keepalive_hours":[22]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy, err := Load(fp)
	if err != nil {
		t.Fatalf("legacy config should load: %v", err)
	}
	if legacy.KeepaliveRefreshWindowDur != 168*time.Hour {
		t.Fatalf("legacy default window=%v want 168h", legacy.KeepaliveRefreshWindowDur)
	}

	for _, raw := range []string{"0s", "-1h", "not-a-duration"} {
		if err := os.WriteFile(fp, []byte(`{"schedule":{"keepalive_refresh_window":"`+raw+`"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(fp); err == nil {
			t.Errorf("window %q should be rejected", raw)
		}
	}
}
