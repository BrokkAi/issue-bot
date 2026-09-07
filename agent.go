package issuebot

import (
	"context"
	"log/slog"

	"github.com/BrokkAi/acp-go"
	"github.com/BrokkAi/acp-go/runner"
)

type Agent interface {
	Execute(context.Context, string) (Result, error)
}
type agentProcess struct {
	config Config
	log    *slog.Logger
}

func (a agentProcess) Execute(ctx context.Context, prompt string) (Result, error) {
	text, err := (runner.Runner{Config: runner.Config{Directory: a.config.Directory, StateDirectory: a.config.StateDirectory, Agent: a.config.Agent, AutoApprove: true, ClientInfo: acp.ClientInfo{Name: "issue-bot", Version: "0.1.0"}}, Log: a.log}).Execute(ctx, prompt)
	if err != nil {
		return Result{}, err
	}
	return parseResult(text)
}
