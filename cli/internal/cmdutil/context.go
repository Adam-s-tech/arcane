package cmdutil

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/getarcaneapp/arcane/cli/v2/internal/client"
	"github.com/getarcaneapp/arcane/cli/v2/internal/output"
	runtimectx "github.com/getarcaneapp/arcane/cli/v2/internal/runtime"
)

// ClientFromCommand returns a configured authenticated client for the command.
func ClientFromCommand(cmd *cobra.Command) (*client.Client, error) {
	if cmd == nil {
		return nil, errors.New("nil command")
	}
	if app, ok := runtimectx.From(cmd.Context()); ok {
		return app.Client()
	}
	return client.NewFromConfig()
}

// UnauthClientFromCommand returns a configured unauthenticated client for the command.
func UnauthClientFromCommand(cmd *cobra.Command) (*client.Client, error) {
	if cmd == nil {
		return nil, errors.New("nil command")
	}
	if app, ok := runtimectx.From(cmd.Context()); ok {
		return app.UnauthClient()
	}
	return client.NewFromConfigUnauthenticated()
}

// JSONOutputEnabled returns true if JSON output is enabled for this command.
func JSONOutputEnabled(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	if flag := cmd.Flags().Lookup("json"); flag != nil && flag.Changed {
		val, err := cmd.Flags().GetBool("json")
		return err == nil && val
	}
	if app, ok := runtimectx.From(cmd.Context()); ok {
		return app.IsJSON()
	}
	return false
}

// AssumeYes returns true when prompts should be skipped.
func AssumeYes(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	if app, ok := runtimectx.From(cmd.Context()); ok {
		return app.AssumeYes()
	}
	return false
}

// Confirm prompts the user unless --yes is enabled.
func Confirm(cmd *cobra.Command, prompt string) (bool, error) {
	if AssumeYes(cmd) {
		return true, nil
	}

	return output.Confirm(cmd.Context(), cmd.InOrStdin(), cmd.ErrOrStderr(), prompt)
}
