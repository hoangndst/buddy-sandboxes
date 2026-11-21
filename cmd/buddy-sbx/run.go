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

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
	"github.com/hoangndst/buddy-sandboxes/pkg/client"
)

func newRunCmd() *cobra.Command {
	var (
		flagName                  string
		flagDetach                bool
		flagRm                    bool
		flagRmExplicit            bool
		flagEnv                   []string
		flagPorts                 []string
		flagDuration              time.Duration
		flagCPU                   string
		flagMemory                string
		flagEntrypoint            string
		flagRuntimeClass          string
		flagGitURL                string
		flagGitBranch             string
		flagGitDepth              int32
		flagGitSubpath            string
		flagPVC                   string
		flagShareProcessNamespace bool
		flagMCPServers            []string
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
			if flagDuration < 0 {
				return fmt.Errorf("--duration cannot be negative")
			}
			if flagDuration > time.Duration(sandboxesv1alpha1.MaxActiveDeadlineSeconds)*time.Second {
				return fmt.Errorf("--duration cannot exceed %d seconds", sandboxesv1alpha1.MaxActiveDeadlineSeconds)
			}

			var mcpServers []apiv1.MCPServerSpec
			for _, mcpFlag := range flagMCPServers {
				server, err := parseMCPServerFlag(mcpFlag)
				if err != nil {
					return err
				}
				mcpServers = append(mcpServers, *server)
			}

			c, err := getClient()
			if err != nil {
				return err
			}

			target := args[0]
			preset := ResolvePresetOrImage(target)

			createReq, execCmd := buildRunConfig(preset, args, flagDetach, flagEntrypoint, flagName,
				flagEnv, flagCPU, flagMemory, flagDuration, flagShareProcessNamespace, flagRuntimeClass,
				flagGitURL, flagGitBranch, flagGitDepth, flagGitSubpath, flagPVC)
			createReq.MCPServers = mcpServers

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			sbx, err := c.CreateSandbox(ctx, createReq)
			if err != nil {
				return fmt.Errorf("failed to create sandbox: %w", err)
			}

			if flagDetach {
				fmt.Println(sbx.ID)
				return nil
			}

			rmOnExit := true
			if flagRmExplicit {
				rmOnExit = flagRm
			}
			if rmOnExit {
				defer func() {
					_ = c.DeleteSandbox(context.Background(), sbx.ID)
				}()
			}

			return runInteractiveSession(ctx, c, sbx.ID, execCmd, flagPorts)
		},
	}

	cmd.Flags().StringVarP(&flagName, "name", "n", "", "Assign a name to the sandbox (RFC 1123)")
	cmd.Flags().BoolVarP(&flagDetach, "detach", "d", false, "Run sandbox in background and print ID")
	cmd.Flags().BoolVar(&flagRm, "rm", true, "Automatically remove the sandbox on exit")
	cmd.Flags().StringArrayVarP(&flagEnv, "env", "e", nil, "Set environment variables (KEY=VAL or KEY)")
	cmd.Flags().StringArrayVarP(&flagPorts, "port", "p", nil, "Publish port(s) [local:]remote (e.g. 3000:3000)")
	cmd.Flags().DurationVar(&flagDuration, "duration", 0, "Execution deadline (e.g. 30m, 1h)")
	cmd.Flags().BoolVar(
		&flagShareProcessNamespace,
		"share-process-namespace",
		false,
		"Share the Pod process namespace to reap orphaned processes",
	)
	cmd.Flags().StringVar(&flagCPU, "cpu", "", "CPU limit (e.g. 500m, 2)")
	cmd.Flags().StringVar(&flagMemory, "memory", "", "Memory limit (e.g. 512Mi, 2Gi)")
	cmd.Flags().StringVar(&flagEntrypoint, "entrypoint", "", "Overwrite the default entrypoint")
	cmd.Flags().StringVar(&flagRuntimeClass, "runtime-class", "", "Container runtime class (e.g. gvisor, kata, runc)")
	cmd.Flags().StringVar(&flagRuntimeClass, "runtime", "", "Alias for --runtime-class")
	cmd.Flags().StringVar(&flagGitURL, "git-url", "", "Git repository URL to seed into workspace")
	cmd.Flags().StringVar(&flagGitBranch, "git-branch", "", "Git branch, tag, or commit to checkout (defaults to HEAD)")
	cmd.Flags().Int32Var(&flagGitDepth, "git-depth", 0, "Git shallow clone depth (default: full clone)")
	cmd.Flags().StringVar(&flagGitSubpath, "git-subpath", "", "Subfolder within /workspace to clone repository into")
	cmd.Flags().StringVar(&flagPVC, "pvc", "", "Name of pre-existing PersistentVolumeClaim to mount at /workspace")
	cmd.Flags().StringArrayVar(&flagMCPServers, "mcp-server", nil,
		"Attach MCP sidecar (format: name=...,image=...,port=...[,mountWorkspace=true])")

	cmd.PreRun = func(c *cobra.Command, args []string) {
		flagRmExplicit = c.Flags().Changed("rm")
	}

	return cmd
}

func buildRunConfig(preset Preset, args []string, detach bool, entrypoint, name string,
	envFlags []string, cpu, memory string, duration time.Duration,
	shareProcessNamespace bool, runtimeClass string, gitURL, gitBranch string, gitDepth int32,
	gitSubpath, pvc string) (*apiv1.CreateSandboxRequest, []string) {

	var containerCmd, containerArgs, execCmd []string

	if detach {
		if entrypoint != "" {
			containerCmd = []string{entrypoint}
		} else if len(preset.Command) > 0 {
			containerCmd = preset.Command
		}
		if len(args) > 1 {
			containerArgs = args[1:]
		}
	} else {
		containerCmd = []string{"/bin/sh", "-c", "trap : TERM INT; sleep infinity & wait"}
		if entrypoint != "" {
			execCmd = []string{entrypoint}
		} else if len(preset.Command) > 0 {
			execCmd = preset.Command
		}
		if len(args) > 1 {
			execCmd = append(execCmd, args[1:]...)
		}
		if len(execCmd) == 0 {
			execCmd = []string{"/bin/sh"}
		}
	}

	envVars, warnings := FilterWhitelistedEnv(preset, envFlags)
	for _, w := range warnings {
		_, _ = fmt.Fprintln(os.Stderr, "WARNING: "+w)
	}

	var resources *apiv1.ResourceRequirements
	if cpu != "" || memory != "" {
		resources = &apiv1.ResourceRequirements{
			Limits:   make(map[string]string),
			Requests: make(map[string]string),
		}
		if cpu != "" {
			resources.Limits["cpu"] = cpu
			resources.Requests["cpu"] = cpu
		}
		if memory != "" {
			resources.Limits["memory"] = memory
			resources.Requests["memory"] = memory
		}
	}

	var maxDurationSec *int64
	if duration > 0 {
		sec := int64(duration / time.Second)
		if duration%time.Second != 0 {
			sec++
		}
		maxDurationSec = &sec
	}

	var rc *string
	if runtimeClass != "" {
		rc = &runtimeClass
	}
	var shareProcessNamespaceRequest *bool
	if shareProcessNamespace {
		shareProcessNamespaceRequest = &shareProcessNamespace
	}

	req := &apiv1.CreateSandboxRequest{
		Name:                  name,
		Image:                 preset.Image,
		Command:               containerCmd,
		Args:                  containerArgs,
		Env:                   envVars,
		MaxDurationSeconds:    maxDurationSec,
		ShareProcessNamespace: shareProcessNamespaceRequest,
		Resources:             resources,
		RuntimeClassName:      rc,
	}

	if gitURL != "" || pvc != "" {
		req.Workspace = &apiv1.WorkspaceSpec{
			Ephemeral: pvc == "",
			MountPath: "/workspace",
		}
		if gitURL != "" {
			req.Workspace.Git = &apiv1.GitRepositorySpec{
				URL:      gitURL,
				Revision: gitBranch,
				Depth:    gitDepth,
				Subpath:  gitSubpath,
			}
		}
		if pvc != "" {
			req.Workspace.PersistentVolumeClaim = &apiv1.PersistentVolumeClaimWorkspace{
				ClaimName: pvc,
			}
		}
	}

	return req, execCmd
}

func runInteractiveSession(ctx context.Context, c *client.Client, id string, execCmd []string,
	ports []string) error {

	_, _ = fmt.Fprintf(os.Stderr, "Waiting for sandbox %s to start...\n", id)

	waitCtx, waitCancel := context.WithTimeout(ctx, 3*time.Minute)
	defer waitCancel()

	readySbx, err := c.WaitForRunning(waitCtx, id, 1*time.Second)
	if err != nil {
		return fmt.Errorf("sandbox failed to start: %w", err)
	}

	cancelPortForwards := startPortForwarders(ctx, c, readySbx.ID, ports)
	defer cancelPortForwards()

	isTTY := term.IsTerminal(int(os.Stdin.Fd()))
	var resizeChan chan apiv1.ResizeMessage
	if isTTY {
		rc, cleanup, err := setupTTY()
		if err == nil {
			defer cleanup()
			resizeChan = rc
		}
	}

	exitCode, execErr := c.Exec(ctx, readySbx.ID, client.ExecOptions{
		Cmd:        execCmd,
		TTY:        isTTY,
		Stdin:      os.Stdin,
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
		ResizeChan: resizeChan,
	})

	if execErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Execution error: %v\n", execErr)
	}

	if exitCode != 0 {
		return ExitCodeError{Code: exitCode}
	}

	return nil
}

func startPortForwarders(ctx context.Context, c *client.Client, id string, ports []string) func() {
	cancels := make([]context.CancelFunc, 0, len(ports))

	for _, portSpec := range ports {
		localPort, remotePort, err := parsePortSpec(portSpec)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "Invalid port spec %q: %v\n", portSpec, err)
			continue
		}

		pfCtx, pfCancel := context.WithCancel(ctx)
		cancels = append(cancels, pfCancel)

		go func(lp, rp int) {
			err := c.PortForward(pfCtx, id, client.PortForwardOptions{
				LocalPort:  lp,
				RemotePort: rp,
			})
			if err != nil && pfCtx.Err() == nil {
				_, _ = fmt.Fprintf(os.Stderr, "Port forward %d:%d error: %v\n", lp, rp, err)
			}
		}(localPort, remotePort)

		_, _ = fmt.Fprintf(os.Stderr, "Forwarding port %d -> %d on sandbox %s\n", localPort, remotePort, id)
	}

	return func() {
		for _, cancel := range cancels {
			cancel()
		}
	}
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

func parseMCPServerFlag(flagVal string) (*apiv1.MCPServerSpec, error) {
	parts := strings.Split(flagVal, ",")
	server := &apiv1.MCPServerSpec{}
	for _, part := range parts {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			return nil, fmt.Errorf("invalid MCP server field %q in %q (expected key=value)", part, flagVal)
		}
		key := strings.TrimSpace(kv[0])
		val := strings.TrimSpace(kv[1])
		switch strings.ToLower(key) {
		case "name":
			server.Name = val
		case "image":
			server.Image = val
		case "port":
			p, err := strconv.ParseInt(val, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("invalid MCP server port %q: %w", val, err)
			}
			server.Port = int32(p)
		case "mountworkspace":
			mw, err := strconv.ParseBool(val)
			if err != nil {
				return nil, fmt.Errorf("invalid mountWorkspace %q: %w", val, err)
			}
			server.MountWorkspace = mw
		case "mountworkspacereadonly":
			ro, err := strconv.ParseBool(val)
			if err != nil {
				return nil, fmt.Errorf("invalid mountWorkspaceReadOnly %q: %w", val, err)
			}
			server.MountWorkspaceReadOnly = ro
		default:
			return nil, fmt.Errorf("unknown MCP server parameter %q", key)
		}
	}
	if server.Name == "" {
		return nil, fmt.Errorf("mcp-server requires 'name'")
	}
	if server.Image == "" {
		return nil, fmt.Errorf("mcp-server requires 'image'")
	}
	if server.Port <= 0 || server.Port > 65535 {
		return nil, fmt.Errorf("mcp-server requires valid 'port' (1-65535)")
	}
	return server, nil
}
