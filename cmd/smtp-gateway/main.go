package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
)

// version is supplied by the build task through linker flags.
var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		slog.Error("Command failed", "error", err)
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("smtp-gateway", flag.ContinueOnError)
	flags.SetOutput(output)
	showVersion := flags.Bool("version", false, "Print the version and exit")

	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument: %s", flags.Arg(0))
	}
	if *showVersion {
		_, err := fmt.Fprintf(output, "smtp-gateway %s\n", version)
		return err
	}

	flags.Usage()
	return nil
}
