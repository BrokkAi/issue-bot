package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	bot "github.com/BrokkAi/issue-bot"
	"github.com/BrokkAi/issue-bot/internal/worker"
)

func workerCommand(ctx context.Context, args []string, version string) error {
	fs := flag.NewFlagSet("bib worker", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: bib worker --socket PATH\n\nServe versioned one-shot issue implementation runs to Brokk Town over a private Unix socket.")
		fs.PrintDefaults()
	}
	socket := fs.String("socket", "", "private Unix-domain socket path (required)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 || *socket == "" {
		return fmt.Errorf("worker requires exactly one --socket PATH")
	}
	return worker.Serve(ctx, *socket, worker.Initialize{
		Protocol: worker.ProtocolVersion, MinimumProtocol: worker.MinimumProtocol,
		Bot: "issue-bot", Version: version, Capabilities: []string{"run", "progress", "issue-result", "exact-issue"},
	}, func(ctx context.Context, request worker.Request, progress func(worker.Progress)) (worker.Result, error) {
		cfg := bot.DefaultConfig()
		cfg.Remote = request.Remote
		cfg.Branch = request.Branch
		cfg.Directory = request.Directory
		cfg.StateDirectory = request.StateDirectory
		cfg.Agent = request.Agent
		cfg.GitHub.Repo = request.Repo
		cfg.GitHub.Host = request.Host
		cfg.Draft = false
		cfg.Verify = request.Verify
		cfg.Issue = request.Issue
		ctx = bot.WithProgress(ctx, func(p bot.Progress) {
			progress(worker.Progress{Phase: p.Phase, Task: p.Task})
		})
		runErr := bot.Run(ctx, cfg, slog.Default(), true)
		result := worker.Result{Issue: &worker.IssueResult{Owned: []worker.IssueOwnership{}}}
		saved, readErr := bot.ReadState(cfg)
		if readErr != nil {
			return result, readErr
		}
		if saved != nil {
			for _, job := range saved.Jobs {
				if job.Status != "submitted" {
					continue
				}
				parsed, err := url.Parse(job.URL)
				if err != nil || parsed.Host != request.Host {
					continue
				}
				parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
				if len(parts) != 4 || !strings.EqualFold(strings.Join(parts[:2], "/"), request.Repo) || parts[2] != "pull" {
					continue
				}
				if number, err := strconv.Atoi(parts[3]); err == nil && number > 0 {
					result.Issue.Owned = append(result.Issue.Owned, worker.IssueOwnership{PR: number, Branch: job.Branch, Issue: job.Issue.Number})
				}
			}
		}
		return result, runErr
	}, slog.Default())
}
