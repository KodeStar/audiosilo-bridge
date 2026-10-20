package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPodcastsDefaultsEnvAndValidation(t *testing.T) {
	defaults, err := Load(t.TempDir())
	if err != nil || defaults.Podcasts.MaxFeedMB != 32 || len(defaults.Podcasts.AllowNetworks) != 0 {
		t.Fatalf("defaults: %+v %v", defaults.Podcasts, err)
	}
	t.Setenv("AUDIOSILO_BRIDGE_PODCASTS_ALLOW_NETWORKS", "127.0.0.1/32, fd00::/8")
	t.Setenv("AUDIOSILO_BRIDGE_PODCASTS_MAX_FEED_MB", "16")
	cfg, err := Load(t.TempDir())
	if err != nil || cfg.Podcasts.MaxFeedMB != 16 || len(cfg.Podcasts.AllowNetworks) != 2 || !cfg.EnvSet(KeyPodcastsAllowNetworks) || !cfg.EnvSet(KeyPodcastsMaxFeedMB) {
		t.Fatalf("env: %+v %v", cfg.Podcasts, err)
	}
	networks, err := cfg.Podcasts.Networks()
	if err != nil || networks[1].String() != "fd00::/8" {
		t.Fatal(networks, err)
	}
	for _, value := range []string{"all", "127.0.0.1/99", "127.0.0.1/32,"} {
		t.Setenv("AUDIOSILO_BRIDGE_PODCASTS_ALLOW_NETWORKS", value)
		if _, err := Load(t.TempDir()); err == nil {
			t.Fatal("invalid allow list accepted", value)
		}
	}
	t.Setenv("AUDIOSILO_BRIDGE_PODCASTS_ALLOW_NETWORKS", "")
	for _, value := range []string{"0", "-1", "oops", "100000"} {
		t.Setenv("AUDIOSILO_BRIDGE_PODCASTS_MAX_FEED_MB", value)
		if _, err := Load(t.TempDir()); err == nil {
			t.Fatal("invalid feed cap accepted", value)
		}
	}
	t.Setenv("AUDIOSILO_BRIDGE_PODCASTS_MAX_FEED_MB", " ")
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, FileName), []byte("podcasts:\n  max_feed_mb: 8\n  allow_networks: [192.168.10.0/24]\n"), 0600)
	cfg, err = Load(dir)
	if err != nil || cfg.Podcasts.MaxFeedMB != 8 || cfg.EnvSet(KeyPodcastsMaxFeedMB) || cfg.EnvSet(KeyPodcastsAllowNetworks) {
		t.Fatal(cfg.Podcasts, err)
	}
}
