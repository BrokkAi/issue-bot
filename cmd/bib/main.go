package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	bot "github.com/BrokkAi/issue-bot"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	log := slog.New(newConsole(os.Stderr))
	for _, arg := range os.Args[1:] {
		if arg == "--json" || arg == "-json" || arg == "--json=true" {
			log = slog.New(slog.NewJSONHandler(os.Stderr, nil))
		}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := execute(ctx, os.Args[1:], log); err != nil && ctx.Err() == nil {
		log.Error("Stopped", "error", err)
		os.Exit(1)
	}
}
func execute(ctx context.Context, args []string, log *slog.Logger) error {
	return executeWithRun(ctx, args, log, bot.Run)
}

type runFunc func(context.Context, bot.Config, *slog.Logger, bool) error

func executeWithRun(ctx context.Context, args []string, log *slog.Logger, run runFunc) error {
	mode := "run"
	if len(args) > 0 {
		switch args[0] {
		case "run", "once", "status", "retry":
			mode = args[0]
			args = args[1:]
		}
	}
	fs := flag.NewFlagSet("bib", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: bib [run|once|status|retry] [repository path or URL] [options]\n\nWalk GitHub issues and prepare pull requests. No config file is required.")
		fs.PrintDefaults()
	}
	file := fs.String("config", "", "optional JSON configuration")
	branch := fs.String("branch", "", "base branch (default: repository default)")
	agent := fs.String("agent", "", "ACP executable (default: codex-acp or npx)")
	model := fs.String("model", "", "agent model ID")
	effort := fs.String("effort", "", "reasoning effort")
	issue := fs.Int("issue", 0, "work on one issue number")
	draft := fs.Bool("draft", true, "create draft pull requests")
	once := fs.Bool("once", mode == "once", "attempt at most one issue, then exit")
	fs.Bool("json", false, "structured logs and status")
	defaults := bot.DefaultConfig()
	poll := fs.Duration("poll", 0, "poll interval, e.g. 5m")
	timeout := fs.Duration("timeout", 0, "budget for each attempt, e.g. 2h")
	claimTimeout := fs.Duration("claim-timeout", 0, "claim expiry without renewal, e.g. 15m")
	attempts := fs.Int("attempts", defaults.Attempts, "maximum attempts per issue")
	var labels, agentArgs []string
	fs.Func("label", "required label; repeat to require all labels", func(s string) error { labels = append(labels, s); return nil })
	fs.Func("agent-arg", "argument to agent; repeat as needed", func(s string) error { agentArgs = append(agentArgs, s); return nil })
	if err := parseInterspersed(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 1 {
		return errors.New("pass one repository path or URL")
	}
	var cfg bot.Config
	var err error
	if *file != "" {
		if fs.NArg() > 0 {
			return errors.New("use a repository argument or --config")
		}
		cfg, err = bot.ReadConfig(*file)
	} else {
		cfg, err = bot.Discover(ctx, fs.Arg(0), *branch)
	}
	if err != nil {
		return err
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "branch":
			cfg.Branch = *branch
		case "agent":
			cfg.Agent.Command = []string{*agent}
		case "model":
			cfg.Agent.Model = *model
			if strings.TrimSpace(*model) == "" {
				err = errors.New("model cannot be empty")
			}
		case "effort":
			cfg.Agent.Effort = *effort
			if strings.TrimSpace(*effort) == "" {
				err = errors.New("effort cannot be empty")
			}
		case "issue":
			cfg.Issue = *issue
		case "draft":
			cfg.Draft = *draft
		case "label":
			cfg.Labels = labels
		case "poll":
			cfg.Poll = bot.Duration(*poll)
		case "timeout":
			cfg.Timeout = bot.Duration(*timeout)
		case "claim-timeout":
			cfg.ClaimTimeout = bot.Duration(*claimTimeout)
		case "attempts":
			cfg.Attempts = *attempts
		}
	})
	if err != nil {
		return err
	}
	cfg.Agent.Command = append(cfg.Agent.Command, agentArgs...)
	if err := cfg.Validate(); err != nil {
		return err
	}
	if cfg.GitHubRepo() == "" {
		return errors.New("GitHub remote required; set github.repo for a local mirror")
	}
	if mode == "status" {
		s, err := bot.ReadState(cfg)
		if err != nil {
			return err
		}
		e := json.NewEncoder(os.Stdout)
		e.SetIndent("", "  ")
		return e.Encode(s)
	}
	if err := bot.ResolveAgent(&cfg, *file == "" && *agent == ""); err != nil {
		return err
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return errors.New("install GitHub CLI and run gh auth login")
	}
	if mode == "retry" {
		if err := bot.Retry(cfg); err != nil {
			return err
		}
	}
	log.Info("Walking GitHub issues", "repository", cfg.GitHubRepo(), "branch", cfg.Branch, "checkout", cfg.Directory, "state", cfg.StateDirectory)
	return run(ctx, cfg, log, *once)
}

// Accept the repository before or after flags, as users expect from CLI tools.
func parseInterspersed(fs *flag.FlagSet, args []string) error {
	var options, positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		options = append(options, arg)
		name := strings.TrimLeft(arg, "-")
		if strings.Contains(name, "=") {
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			continue
		}
		boolean, ok := f.Value.(interface{ IsBoolFlag() bool })
		if ok && boolean.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			i++
			options = append(options, args[i])
		}
	}
	return fs.Parse(append(append(options, "--"), positional...))
}
