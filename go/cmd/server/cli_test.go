package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The ordinary Go test binary runs the actual entry point in a child process,
// so assertions cover os.Exit, stdout and stderr without building another app.
func TestCLIProcessHelper(t *testing.T) {
	if os.Getenv("WEAVE_CLI_TEST_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"server"}, os.Args[i+1:]...)
			main()
			return
		}
	}
	t.Fatal("missing child-process argument separator")
}

func runCLIProcess(t *testing.T, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, append([]string{"-test.run=^TestCLIProcessHelper$", "--"}, args...)...)
	command.Env = append(os.Environ(), "WEAVE_CLI_TEST_PROCESS=1")
	var out, diagnostic bytes.Buffer
	command.Stdout, command.Stderr = &out, &diagnostic
	err = command.Run()
	if ctx.Err() != nil {
		t.Fatalf("CLI did not exit promptly: %v", ctx.Err())
	}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
		return out.String(), diagnostic.String(), exit.ExitCode()
	}
	return out.String(), diagnostic.String(), 0
}

func TestCLIInvalidConfigurationDiagnostics(t *testing.T) {
	for _, scenario := range []struct {
		name string
		args []string
		want []string
	}{
		{"zero requests", []string{"--rate-limit-requests=0"}, []string{"--rate-limit-requests=0", "between 1 and 9223372036854775807"}},
		{"negative requests", []string{"--rate-limit-requests=-2"}, []string{"--rate-limit-requests=-2", "between 1 and 9223372036854775807"}},
		{"zero period", []string{"--rate-limit-period=0s"}, []string{"--rate-limit-period=0s", "1ns", "9223372036854775807ns"}},
		{"negative period", []string{"--rate-limit-period=-1s"}, []string{"--rate-limit-period=-1s", "1ns"}},
		{"negative capacity", []string{"--rate-limit-capacity=-3"}, []string{"--rate-limit-capacity=-3", "between 1 and 9223372036854775807"}},
		{"multiple fields", []string{"--rate-limit-requests=0", "--rate-limit-period=-1s", "--rate-limit-capacity=-3"}, []string{"--rate-limit-requests=0", "--rate-limit-period=-1s", "--rate-limit-capacity=-3"}},
		{"too fast", []string{"--rate-limit-requests=2", "--rate-limit-period=1ns"}, []string{"--rate-limit-requests=2", "maximum 1", "--rate-limit-period=1ns", "reduce requests or increase period"}},
		{"capacity overflow", []string{"--rate-limit-capacity=153722868"}, []string{"--rate-limit-capacity=153722868", "maximum 153722867", "--rate-limit-period=1m0s", "9223372036854775807", "reduce capacity or period"}},
		{"malformed count", []string{"--rate-limit-requests=NaN"}, []string{`invalid value "NaN" for flag -rate-limit-requests`, "whole integer", "9223372036854775807"}},
		{"integer overflow", []string{"--rate-limit-capacity=9223372036854775808"}, []string{`invalid value "9223372036854775808" for flag -rate-limit-capacity`, "whole integer", "9223372036854775807"}},
		{"malformed period", []string{"--rate-limit-period=banana"}, []string{`invalid value "banana" for flag -rate-limit-period`, "duration", "1s", "9223372036854775807ns"}},
		{"period precision", []string{"--rate-limit-period=1.5ns"}, []string{`invalid value "1.5ns" for flag -rate-limit-period`, "whole nanoseconds", "1ns"}},
		{"period overflow", []string{"--rate-limit-period=2562048h"}, []string{`invalid value "2562048h" for flag -rate-limit-period`, "at most 2562047h47m16.854775807s", "9223372036854775807ns"}},
		{"unknown flag", []string{"--does-not-exist=1"}, []string{"flag provided but not defined: -does-not-exist"}},
		{"missing value", []string{"--rate-limit-capacity"}, []string{"flag needs an argument: -rate-limit-capacity"}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			data := filepath.Join(t.TempDir(), "must-not-open")
			args := append([]string{"--data=" + data, "--addr=127.0.0.1:0"}, scenario.args...)
			stdout, stderr, code := runCLIProcess(t, args...)
			if code != 1 || stdout != "" {
				t.Fatalf("invalid CLI must exit 1 with empty stdout: code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if strings.Count(stderr, "Server:") != 1 || strings.Contains(stderr, "Usage of") {
				t.Fatalf("expected one failure diagnostic without duplicate usage: %q", stderr)
			}
			for _, want := range scenario.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("diagnostic %q does not explain %q", stderr, want)
				}
			}
			if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid CLI opened data: %v", err)
			}
			if strings.Contains(stderr, "Local demo") {
				t.Fatal("invalid CLI reached startup output")
			}
		})
	}
}

func TestCLIHelpAndSeedSuccessStreams(t *testing.T) {
	for _, flag := range []string{"--help", "-h"} {
		t.Run(flag, func(t *testing.T) {
			data := filepath.Join(t.TempDir(), "must-not-open")
			stdout, stderr, code := runCLIProcess(t, "--data="+data, flag)
			if code != 0 || stderr != "" || strings.Count(stdout, "Usage of server:") != 1 {
				t.Fatalf("help must exit 0 and write only stdout: code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			for _, name := range []string{"-addr", "-health-addr", "-actors", "-rate-limit-requests", "-rate-limit-period", "-rate-limit-capacity"} {
				if !strings.Contains(stdout, name) {
					t.Errorf("help omits %s", name)
				}
			}
			if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("help opened data: %v", err)
			}
		})
	}
	for _, scenario := range []struct {
		name string
		args []string
	}{
		{"defaults", nil},
		{"minimum interval", []string{"--rate-limit-requests=1", "--rate-limit-period=1ns"}},
		{"maximum count and period", []string{"--rate-limit-requests=9223372036854775807", "--rate-limit-period=9223372036854775807ns"}},
	} {
		t.Run("successful seed/"+scenario.name, func(t *testing.T) {
			args := append([]string{"--data=" + t.TempDir(), "--seed", "--contacts=0", "--files=0"}, scenario.args...)
			stdout, stderr, code := runCLIProcess(t, args...)
			// Seed progress legitimately uses stderr; it must not contain a CLI failure.
			if code != 0 || stdout != "Done seeding.\n" || strings.Contains(stderr, "Server:") {
				t.Fatalf("successful seed CLI regressed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
		})
	}
}

func TestRateLimitIntegerParsingPreservesFlagSyntax(t *testing.T) {
	for _, scenario := range []struct {
		value string
		want  int64
	}{
		{"10", 10}, {"+10", 10}, {"010", 8}, {"0x10", 16}, {"1_000", 1000},
		{"0", 0}, {"-1", -1}, {"9223372036854775807", 9223372036854775807},
	} {
		got, err := parseRateLimitInteger(scenario.value)
		if err != nil || got != scenario.want {
			t.Errorf("parse %q: got %d, %v; want %d", scenario.value, got, err, scenario.want)
		}
	}
	for _, value := range []string{"NaN", "Inf", "1.5", "9223372036854775808", "-9223372036854775809"} {
		if _, err := parseRateLimitInteger(value); err == nil || !strings.Contains(err.Error(), "whole integer between 1 and 9223372036854775807") {
			t.Errorf("parse %q must explain required integer range: %v", value, err)
		}
	}
}

type failedHelpWriter struct{ err error }

func (w failedHelpWriter) Write([]byte) (int, error) { return 0, w.err }

func TestHelpOutputFailureIsReportedWithoutOpeningResources(t *testing.T) {
	data := filepath.Join(t.TempDir(), "must-not-open")
	want := errors.New("synthetic output failure")
	err := run(t.Context(), []string{"--data=" + data, "--help"}, failedHelpWriter{want})
	if !errors.Is(err, want) || !strings.Contains(err.Error(), "write help") {
		t.Fatalf("help must not succeed when its output fails: %v", err)
	}
	if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed help opened data: %v", err)
	}
}
