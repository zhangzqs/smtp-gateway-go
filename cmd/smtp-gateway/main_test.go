package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRun(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name       string
		args       []string
		wantOutput string
		wantError  string
	}{
		{
			name:       "default help",
			wantOutput: "Usage of smtp-gateway:",
		},
		{
			name:       "help",
			args:       []string{"--help"},
			wantOutput: "Usage of smtp-gateway:",
		},
		{
			name:       "short help",
			args:       []string{"-h"},
			wantOutput: "Usage of smtp-gateway:",
		},
		{
			name:       "version",
			args:       []string{"--version"},
			wantOutput: "smtp-gateway dev\n",
		},
		{
			name:      "unknown flag",
			args:      []string{"--unknown"},
			wantError: "flag provided but not defined",
		},
		{
			name:      "unexpected argument",
			args:      []string{"serve"},
			wantError: "unexpected argument: serve",
		},
		{
			name:      "argument after version",
			args:      []string{"--version", "serve"},
			wantError: "unexpected argument: serve",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			var output bytes.Buffer
			err := run(testCase.args, &output)
			if testCase.wantError != "" {
				require.ErrorContains(t, err, testCase.wantError)
				return
			}
			require.NoError(t, err)
			require.Contains(t, output.String(), testCase.wantOutput)
		})
	}
}
