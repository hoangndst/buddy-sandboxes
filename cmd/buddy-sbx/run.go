package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
	"github.com/hoangndst/buddy-sandboxes/pkg/client"
)

func newRunCmd() *cobra.Command {
	var (
		flagName       string
		flagDetach     bool
		flagRm         bool
		flagRmExplicit bool
		flagEnv        []string
		flagPorts      []string
		flagDuration   time.Duration
		flagCPU        string
		flagMemory     string
		flagEntrypoint string
	)

	cmd := &cobra.Command{
		Use:   "run <preset|image> [flags] [-- command...]",
		Short: "Run an AI agent or container in an isolated sandbox",
		Long: `Run provisions an isolated sandbox, optionally forwards API credentials, 
attaches an interactive pseudo-terminal, and handles port forwarding.

Supported presets:
  claude  - Runs Claude Code assistant with ANTHROPIC_API_KEY
  codex   - Runs OpenAI Codex assistant with OPENAI_API_KEY
  aider   - Runs Aider coding assistant with detected API keys
  shell   - Opens an interactive bash shell in an isolated environment`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := getClient()
			if err != nil {
				return err
			}

			target := args[0]
			preset := ResolvePresetOrImage(target)

			// Determine command and args
			var runCmd []string
			var runArgs []string

			if flagEntrypoint != "" {
				runCmd = []string{flagEntrypoint}
			} else if len(preset.Command) > 0 {
				runCmd = preset.Command
			}

			if len(args) > 1 {
				runArgs = args[1:]
			}

			// Filter environment variables per strict security whitelist (DEC-005)
			envVars, warnings := FilterWhitelistedEnv(preset, flagEnv)
			for _, w := range warnings {
				_, _ = fmt.Fprintln(os.Stderr, "WARNING: "+w)
			}

			// Resource requirements
			var resources *apiv1.ResourceRequirements
			if flagCPU != "" || flagMemory != "" {
				resources = &apiv1.ResourceRequirements{
					Limits:   make(map[string]string),
					Requests: make(map[string]string),
				}
				if flagCPU != "" {
					resources.Limits["cpu"] = flagCPU
					resources.Requests["cpu"] = flagCPU
				}
				if flagMemory != "" {
					resources.Limits["memory"] = flagMemory
					resources.Requests["memory"] = flagMemory
				}
			}

			var maxDurationSec *int64
			if flagDuration > 0 {
				sec := int64(flagDuration.Seconds())
				maxDurationSec = &sec
			}

			createReq := &apiv1.CreateSandboxRequest{
				Name:               flagName,
				Image:              preset.Image,
				Command:            runCmd,
				Args:               runArgs,
				Env:                envVars,
				MaxDurationSeconds: maxDurationSec,
				Resources:          resources,
			}

			ctx := context.Background()
			sbx, err := c.CreateSandbox(ctx, createReq)
			if err != nil {
				return fmt.Errorf("failed to create sandbox: %w", err)
			}

			// Detached mode: print ID and exit
			if flagDetach {
				fmt.Println(sbx.ID)
				return nil
			}

			// Determine --rm default: true for interactive, false for detached
			rmOnExit := true
			if flagRmExplicit {
				rmOnExit = flagRm
			}

			_, _ = fmt.Fprintf(os.Stderr, "Waiting for sandbox %s to start...\n", sbx.ID)

			waitCtx, waitCancel := context.WithTimeout(ctx, 3*time.Minute)
			defer waitCancel()

			readySbx, err := c.WaitForRunning(waitCtx, sbx.ID, 1*time.Second)
			if err != nil {
				if rmOnExit {
					_ = c.DeleteSandbox(context.Background(), sbx.ID)
				}
				return fmt.Errorf("sandbox failed to start: %w", err)
			}

			// Start background port forwarders if requested
			pfCancelFuncs := make([]context.CancelFunc, 0, len(flagPorts))
			defer func() {
				for _, cancel := range pfCancelFuncs {
					cancel()
				}
			}()

			for _, portSpec := range flagPorts {
				localPort, remotePort, err := parsePortSpec(portSpec)
				if err != nil {
					_, _ = fmt.Fprintf(os.Stderr, "Invalid port spec %q: %v\n", portSpec, err)
					continue
				}

				pfCtx, pfCancel := context.WithCancel(ctx)
				pfCancelFuncs = append(pfCancelFuncs, pfCancel)

				go func(lp, rp int) {
					err := c.PortForward(pfCtx, readySbx.ID, client.PortForwardOptions{
						LocalPort:  lp,
						RemotePort: rp,
					})
					if err != nil && pfCtx.Err() == nil {
						_, _ = fmt.Fprintf(os.Stderr, "Port forward %d:%d error: %v\n", lp, rp, err)
					}
				}(localPort, remotePort)

				_, _ = fmt.Fprintf(os.Stderr, "Forwarding port %d -> %d on sandbox %s\n", localPort, remotePort, readySbx.ID)
			}

			// Interactive TTY execution
			isTTY := term.IsTerminal(int(os.Stdin.Fd()))
			var resizeChan chan apiv1.ResizeMessage

			if isTTY {
				oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
				if err == nil {
					defer func() {
						_ = term.Restore(int(os.Stdin.Fd()), oldState)
					}()
				}

				resizeChan = make(chan apiv1.ResizeMessage, 8)
				if w, h, err := term.GetSize(int(os.Stdin.Fd())); err == nil {
					resizeChan <- apiv1.ResizeMessage{Cols: w, Rows: h, Width: uint16(w), Height: uint16(h)}
				}

				sigChan := make(chan os.Signal, 2)
				signal.Notify(sigChan, syscall.SIGWINCH)
				defer signal.Stop(sigChan)

				go func() {
					for range sigChan {
						if w, h, err := term.GetSize(int(os.Stdin.Fd())); err == nil {
							select {
							case resizeChan <- apiv1.ResizeMessage{Cols: w, Rows: h, Width: uint16(w), Height: uint16(h)}:
							default:
							}
						}
					}
				}()
			}

			execCmd := runCmd
			if len(runArgs) > 0 {
				execCmd = append(execCmd, runArgs...)
			}
			if len(execCmd) == 0 {
				execCmd = []string{"/bin/sh"}
			}

			exitCode, execErr := c.Exec(ctx, readySbx.ID, client.ExecOptions{
				Cmd:        execCmd,
				TTY:        isTTY,
				Stdin:      os.Stdin,
				Stdout:     os.Stdout,
				Stderr:     os.Stderr,
				ResizeChan: resizeChan,
			})

			if rmOnExit {
				_ = c.DeleteSandbox(context.Background(), readySbx.ID)
			}

			if execErr != nil {
				_, _ = fmt.Fprintf(os.Stderr, "Execution error: %v\n", execErr)
			}

			if exitCode != 0 {
				os.Exit(exitCode)
			}

			return nil
		},
	}

	cmd.Flags().StringVarP(&flagName, "name", "n", "", "Assign a name to the sandbox (RFC 1123)")
	cmd.Flags().BoolVarP(&flagDetach, "detach", "d", false, "Run sandbox in background and print ID")
	cmd.Flags().BoolVar(&flagRm, "rm", true, "Automatically remove the sandbox on exit")
	cmd.Flags().StringArrayVarP(&flagEnv, "env", "e", nil, "Set environment variables (KEY=VAL or KEY)")
	cmd.Flags().StringArrayVarP(&flagPorts, "port", "p", nil, "Publish port(s) [local:]remote (e.g. 3000:3000)")
	cmd.Flags().DurationVar(&flagDuration, "duration", 0, "Execution deadline (e.g. 30m, 1h)")
	cmd.Flags().StringVar(&flagCPU, "cpu", "", "CPU limit (e.g. 500m, 2)")
	cmd.Flags().StringVar(&flagMemory, "memory", "", "Memory limit (e.g. 512Mi, 2Gi)")
	cmd.Flags().StringVar(&flagEntrypoint, "entrypoint", "", "Overwrite the default entrypoint")

	// Track whether --rm was explicitly set by user
	cmd.PreRun = func(c *cobra.Command, args []string) {
		flagRmExplicit = c.Flags().Changed("rm")
	}

	return cmd
}

func parsePortSpec(spec string) (int, int, error) {
	parts := strings.Split(spec, ":")
	if len(parts) == 1 {
		p, err := strconv.Atoi(parts[0])
		if err != nil || p < 1 || p > 65535 {
			return 0, 0, fmt.Errorf("invalid port %q", parts[0])
		}
		return p, p, nil
	}
	if len(parts) == 2 {
		local, err1 := strconv.Atoi(parts[0])
		remote, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil || local < 1 || local > 65535 || remote < 1 || remote > 65535 {
			return 0, 0, fmt.Errorf("invalid port specification %q", spec)
		}
		return local, remote, nil
	}
	return 0, 0, fmt.Errorf("format must be [local:]remote")
}
