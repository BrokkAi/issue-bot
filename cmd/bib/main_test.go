package main

import (
	"context"
	bot "github.com/BrokkAi/issue-bot"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCLIOverridesAndInterspersedFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"remote":"https://github.com/o/r.git","agent":{"command":["sh"]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	called := false
	run := func(_ context.Context, c bot.Config, _ *slog.Logger, once bool) error {
		called = true
		if c.ClaimTimeout != bot.Duration(2*time.Minute) || !once || c.Issue != 7 || c.Draft || c.Agent.Model != "fixture" || c.Agent.Effort != "low" || len(c.Labels) != 2 {
			t.Fatalf("wrong settings %+v", c)
		}
		return nil
	}
	err := executeWithRun(context.Background(), []string{"once", "--config", path, "--issue", "7", "--claim-timeout", "2m", "--draft=false", "--model", "fixture", "--effort", "low", "--label", "bug", "--label", "ready"}, slog.New(slog.NewTextHandler(io.Discard, nil)), run)
	if err != nil || !called {
		t.Fatalf("CLI %v %v", called, err)
	}
}
