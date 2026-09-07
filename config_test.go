package issuebot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConfigPathsAndStrictJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.json")
	for _, raw := range []string{`{"remote":"https://github.com/o/r.git","unknown":true}`, `{"remote":"https://github.com/o/r.git"} {}`, `{"remote":"https://github.com/o/r.git","poll":"0s"}`, `{"remote":"https://github.com/o/r.git","directory":"x","state_directory":"x/sub"}`} {
		writeTestFile(t, path, raw)
		if _, err := ReadConfig(path); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	writeTestFile(t, path, `{"remote":"https://github.com/o/r.git","agent":{"model":"fixture"}}`)
	cfg, err := ReadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GitHubRepo() != "o/r" || !filepath.IsAbs(cfg.Directory) || cfg.Agent.Command[0] != "codex-acp" || !cfg.Draft {
		t.Fatalf("bad defaults: %+v", cfg)
	}
	if err := os.Symlink("checkout", filepath.Join(filepath.Dir(path), "alias")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, `{"remote":"https://github.com/o/r.git","directory":"checkout","state_directory":"alias/state"}`)
	if _, err := ReadConfig(path); err == nil {
		t.Fatal("symlink overlap accepted")
	}
}
func TestEligibilityAndReceipts(t *testing.T) {
	cfg := DefaultConfig()
	i := Issue{Number: 1, State: "open"}
	if !eligible(cfg, i) {
		t.Fatal("open issue not eligible")
	}
	i.PullRequest = []byte(`{}`)
	if eligible(cfg, i) {
		t.Fatal("PR selected")
	}
	i.PullRequest = nil
	i.Locked = true
	if eligible(cfg, i) {
		t.Fatal("locked issue selected")
	}
	i.Locked = false
	i.Labels = append(i.Labels, struct {
		Name string `json:"name"`
	}{"WONTFIX"})
	if eligible(cfg, i) {
		t.Fatal("excluded label selected")
	}
	for _, raw := range []string{"no receipt", `ISSUE_RESULT {"status":"solved","title":"X","detail":"fixed"}`, "ISSUE_RESULT {\"status\":\"blocked\",\"detail\":\"need info\"}\ntrailing"} {
		if _, err := parseResult(raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err := parseResult(`ISSUE_RESULT {"status":"blocked","detail":"Need a reproduction"}`); err != nil {
		t.Fatal(err)
	}
}
