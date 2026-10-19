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
			wantOutput: "A persistent SMTP relay gateway",
		},
		{
			name:       "help",
			args:       []string{"--help"},
			wantOutput: "A persistent SMTP relay gateway",
		},
		{
			name:       "short help",
			args:       []string{"-h"},
			wantOutput: "A persistent SMTP relay gateway",
		},
		{
			name:       "version",
			args:       []string{"--version"},
			wantOutput: "smtp-gateway dev\n",
		},
		{
			name:      "unknown flag",
			args:      []string{"--unknown"},
			wantError: "unknown flag",
		},
		{
			name:      "serve requires valid configuration",
			args:      []string{"serve", "--config", "missing.yaml"},
			wantError: "missing.yaml",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			var output bytes.Buffer
			err := run(t.Context(), testCase.args, &output)
			if testCase.wantError != "" {
				require.ErrorContains(t, err, testCase.wantError)
				return
			}
			require.NoError(t, err)
			require.Contains(t, output.String(), testCase.wantOutput)
		})
	}
}
