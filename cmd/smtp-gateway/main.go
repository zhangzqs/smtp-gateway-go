package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/zhangzqs/smtp-gateway-go/internal/app"
	"github.com/zhangzqs/smtp-gateway-go/internal/config"
)

// version is supplied by the build task through linker flags.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()
	if err != nil {
		log.Error().Err(err).Msg("command failed")
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
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Start the SMTP gateway",
		Args:  cobra.NoArgs,
	}
	var configPath string
	serve.Flags().StringVar(&configPath, "config", "config.yaml", "configuration file")
	serve.RunE = func(command *cobra.Command, _ []string) error {
		cfg, key, err := config.Load(configPath)
		if err != nil {
			return err
		}
		return app.New(cfg, key).Run(ctx)
	}
	command.AddCommand(serve)
	return command.ExecuteContext(ctx)
}
