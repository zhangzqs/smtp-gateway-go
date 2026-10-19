package main

import (
	"context"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

// version is supplied by the build task through linker flags.
var version = "dev"

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		slog.Error("Command failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	command := &cobra.Command{
		Use:           "smtp-gateway",
		Short:         "A persistent SMTP relay gateway",
		Version:       version,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(command *cobra.Command, _ []string) error {
			return command.Help()
		},
	}
	command.SetVersionTemplate("{{.Name}} {{.Version}}\n")
	command.SetOut(output)
	command.SetErr(output)
	command.SetArgs(args)
	return command.ExecuteContext(ctx)
}
