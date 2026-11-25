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

	sandboxesv1alpha1 "github.com/hoangndst/buddy-sandboxes/api/v1alpha1"
	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
	"github.com/hoangndst/buddy-sandboxes/pkg/client"
)

//nolint:gocyclo // Flag validation and dispatch are intentionally kept with the Cobra command definition.
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
		flagClone                 string
		flagBranch                string
		flagGitSecretRef          string
		flagGitDepth              int32
		flagGitSubpath            string
		flagPVC                   string
		flagShareProcessNamespace bool
		flagMCPServers            []string
		flagAllowedDomains        []string
		flagPreview               bool
		flagIdleCommand           string
		flagIdleArgs              []string
		flagModel                 string
		flagNoSync                bool
		flagReadOnly              bool
	)

	cmd := &cobra.Command{
		Use:   "run <preset|image> [local-path] [flags] [-- task args...]",
		Short: "Run an AI agent or container in an isolated sandbox",
		Long: `Run provisions an isolated sandbox, streams the selected agent's terminal to the local terminal,
and handles port forwarding. When a local path is supplied, buddy-sbx uploads a filtered project snapshot
and synchronizes changes back after the agent exits. Codex project runs use the in-cluster provider broker.
Persistent project runs are retained by default. Detached runs start an idle container, not an agent process.

Supported presets:
  claude  - Runs Claude Code assistant
  codex   - Runs Buddy's version-pinned Codex image
  aider   - Runs Aider coding assistant
  shell   - Opens an interactive bash shell in an isolated environment`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			target, projectPath, taskArgs, err := splitRunArguments(args, cmd.Flags().ArgsLenAtDash())
			if err != nil {
				return err
			}
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

			preset := ResolvePresetOrImage(target)
			if flagReadOnly {
				return fmt.Errorf("--read-only is invalid: container workspaces require write access " +
					"for git initialization and agent caches; use --no-sync to prevent modifications to your local project directory")
			}
			if flagModel != "" && !strings.EqualFold(target, "codex") &&
				!strings.EqualFold(target, "claude") && !strings.EqualFold(target, "aider") {
				return fmt.Errorf("--model is only supported for AI agent presets (codex, claude, aider)")
			}
			if flagClone != "" && flagGitURL != "" {
				return fmt.Errorf("--clone and --git-url cannot be used together")
			}
			if flagBranch != "" && flagGitBranch != "" {
				return fmt.Errorf("--branch and --git-branch cannot be used together")
			}
			gitURL := flagClone
			if gitURL == "" {
				gitURL = flagGitURL
			}
			gitBranch := flagBranch
			if gitBranch == "" {
				gitBranch = flagGitBranch
			}
			cloneMode := flagClone != ""
			if cloneMode || (gitURL != "" && flagGitSecretRef != "") {
				if err := validateGitCloneURL(gitURL); err != nil {
					return err
				}
			}
			if err := validateGitSubpath(flagGitSubpath); err != nil {
				return err
			}
			gitOptionsNeedURL := gitBranch != "" || cmd.Flags().Changed("git-depth") ||
				flagGitSubpath != "" || flagGitSecretRef != ""
			if gitOptionsNeedURL && gitURL == "" {
				return fmt.Errorf("branch, Git depth, subpath, and Git Secret reference require --clone or --git-url")
			}
			if cloneMode && (projectPath != "" || flagPreview || flagDetach || flagPVC != "") {
				return fmt.Errorf("--clone cannot be combined with a local project path, --preview, --detach, or --pvc")
			}
			if projectPath != "" && (flagPVC != "" || gitURL != "" || flagGitBranch != "" || flagBranch != "" ||
				cmd.Flags().Changed("git-depth") || flagGitSubpath != "" || flagGitSecretRef != "") {
				return fmt.Errorf("project snapshots cannot be combined with --pvc or --git-* workspace seeding flags")
			}
			if !flagDetach && (flagIdleCommand != "" || len(flagIdleArgs) != 0) {
				return fmt.Errorf("--idle-command and --idle-arg require --detach")
			}
			if requiresCustomProjectCommand(projectPath, preset, flagEntrypoint, taskArgs, flagPreview, flagDetach) {
				return fmt.Errorf("a custom image project run requires a command after -- or --entrypoint")
			}
			for _, env := range flagEnv {
				name := env
				if i := strings.IndexByte(env, '='); i >= 0 {
					name = env[:i]
				}
				if name == providerModeEnv {
					return fmt.Errorf("%s is reserved for provider configuration", providerModeEnv)
				}
				if name == openAIAPIKeyEnv || name == anthropicAPIKeyEnv {
					return fmt.Errorf("%s is managed by Buddy; use buddy-sbx secret set", name)
				}
			}

			presetForCreate := preset
			presetForCreate.RequiredEnv = nil
			buildArgs := []string{target}
			if projectPath == "" && !flagDetach && !flagPreview {
				buildArgs = append(buildArgs, taskArgs...)
			}
			createReq := buildRunConfig(presetForCreate, buildArgs, flagDetach, flagEntrypoint, flagName,
				flagEnv, flagCPU, flagMemory, flagDuration, flagShareProcessNamespace, flagRuntimeClass,
				gitURL, gitBranch, flagGitDepth, flagGitSubpath, flagPVC)
			if createReq.Workspace != nil && createReq.Workspace.Git != nil {
				createReq.Workspace.Git.SecretRef = flagGitSecretRef
				if flagGitSubpath != "" {
					createReq.WorkingDir = "/workspace/" + flagGitSubpath
				}
			}
			createReq.MCPServers = mcpServers
			if strings.EqualFold(target, "codex") {
				createReq.Provider = providerOpenAI
			}
			if strings.EqualFold(target, "claude") {
				createReq.Provider = providerAnthropic
			}
			if flagModel != "" {
				createReq.Model = flagModel
			}
			if len(flagAllowedDomains) > 0 {
				if createReq.Network == nil {
					createReq.Network = &apiv1.NetworkSpec{
						AllowInternetWeb: true, AllowDNS: true, BlockClusterInternal: true, BlockMetadataService: true,
					}
				}
				createReq.Network.AllowedDomains = flagAllowedDomains
			}

			c, err := getClient()
			if err != nil {
				return err
			}
			agentCommand := BuildAgentCommand(preset, taskArgs, flagModel, flagEntrypoint)
			if projectPath != "" || flagPreview {
				removeOnExit := len(taskArgs) > 0
				if flagRmExplicit {
					removeOnExit = flagRm
				}
				return runProjectWorkflow(ctx, c, projectRunOptions{
					Path: projectPath, TaskArgs: taskArgs, Preview: flagPreview,
					Detach: flagDetach,
					Remove: removeOnExit, Duration: flagDuration, Name: flagName,
					IdleCommand: flagIdleCommand, IdleArgs: flagIdleArgs, EntryPoint: flagEntrypoint,
					Ports: flagPorts, EnvFlags: flagEnv, Preset: preset, CreateRequest: createReq,
					AgentCommand: agentCommand, NoSync: flagNoSync,
				})
			}

			sbx, err := c.CreateSandbox(ctx, createReq)
			if err != nil {
				return fmt.Errorf("failed to create sandbox: %w", err)
			}

			if flagDetach {
				if len(taskArgs) > 0 {
					waitCtx, waitCancel := context.WithTimeout(ctx, 60*time.Second)
					_, err := c.WaitForRunning(waitCtx, sbx.ID, time.Second)
					waitCancel()
					if err != nil {
						return fmt.Errorf("wait for sandbox running: %w", err)
					}
					sessionCmd := taskArgs
					if flagEntrypoint != "" {
						sessionCmd = append([]string{flagEntrypoint}, taskArgs...)
					} else if len(agentCommand) > 0 {
						sessionCmd = agentCommand
					}
					sess, err := c.CreateSession(ctx, sbx.ID, &apiv1.CreateSessionRequest{
						Command:    sessionCmd,
						WorkingDir: createReq.WorkingDir,
					})
					if err != nil {
						return fmt.Errorf("failed to create detached session: %w", err)
					}
					_, _ = fmt.Fprintf(
						os.Stdout,
						"Sandbox %s ready. Session %s started in background.\nTo attach: buddy-sbx attach %s\n",
						sbx.ID,
						sess.ID,
						sbx.ID,
					)
				} else {
					fmt.Println(sbx.ID)
				}
				return nil
			}

			removeOnExit := len(taskArgs) > 0
			if cloneMode {
				removeOnExit = false
			}
			if flagRmExplicit {
				removeOnExit = flagRm
			}

			sessionErr := runExecutionSession(ctx, c, sbx.ID, agentCommand, flagPorts, len(taskArgs) > 0)
			if removeOnExit && (sessionErr == nil || (flagRmExplicit && flagRm)) {
				_ = c.DeleteSandbox(context.Background(), sbx.ID)
			}
			return sessionErr
		},
	}

	cmd.Flags().StringVarP(&flagName, "name", "n", "", "Assign a name to the sandbox (RFC 1123)")
	cmd.Flags().BoolVarP(&flagDetach, "detach", "d", false, "Run sandbox in background and print ID")
	cmd.Flags().BoolVar(&flagRm, "rm", false, "Remove the sandbox when the attached run ends")
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
	cmd.Flags().StringVar(&flagClone, "clone", "", "Clone a remote HTTPS repository into the sandbox workspace")
	cmd.Flags().StringVar(&flagBranch, "branch", "", "Branch, tag, or commit to check out when using --clone")
	cmd.Flags().StringVar(&flagGitSecretRef, "git-secret-ref", "",
		"Same-namespace Kubernetes Secret bound to this HTTPS repository")
	cmd.Flags().StringVar(&flagGitURL, "git-url", "", "Deprecated: seed a Git repository into workspace")
	cmd.Flags().StringVar(&flagGitBranch, "git-branch", "", "Git branch, tag, or commit to checkout (defaults to HEAD)")
	cmd.Flags().Int32Var(&flagGitDepth, "git-depth", 0, "Git shallow clone depth (default: full clone)")
	cmd.Flags().StringVar(&flagGitSubpath, "git-subpath", "", "Subfolder within /workspace to clone repository into")
	cmd.Flags().StringVar(&flagPVC, "pvc", "", "Name of pre-existing PersistentVolumeClaim to mount at /workspace")
	cmd.Flags().StringArrayVar(&flagMCPServers, "mcp-server", nil,
		"Attach MCP sidecar (format: name=...,image=...,port=...[,mountWorkspace=true])")
	cmd.Flags().StringArrayVar(&flagAllowedDomains, "allow-domain", nil,
		"Permit egress to destination domain (e.g. github.com, *.npmjs.org)")
	cmd.Flags().BoolVar(&flagPreview, "preview", false,
		"Print the project snapshot paths and size without creating a sandbox")
	cmd.Flags().StringVar(&flagIdleCommand, "idle-command", "", "Keeper executable for detached custom images")
	cmd.Flags().StringArrayVar(&flagIdleArgs, "idle-arg", nil,
		"Argument for the detached custom-image keeper (repeatable)")
	cmd.Flags().StringVarP(&flagModel, "model", "m", "", "Model override for AI agent presets (e.g. gpt-4o, o3-mini)")
	cmd.Flags().BoolVar(&flagNoSync, "no-sync", false,
		"Execute without synchronizing workspace modifications back to the local repository")
	cmd.Flags().BoolVar(&flagReadOnly, "read-only", false,
		"Deprecated: use --no-sync to prevent modifications to local directory")
	_ = cmd.Flags().MarkHidden("read-only")

	cmd.PreRun = func(c *cobra.Command, args []string) {
		flagRmExplicit = c.Flags().Changed("rm")
	}

	return cmd
}

func requiresCustomProjectCommand(
	projectPath string, preset Preset, entrypoint string, taskArgs []string, preview, detach bool,
) bool {
	return projectPath != "" && len(preset.Command) == 0 && entrypoint == "" && len(taskArgs) == 0 && !preview && !detach
}

func buildRunConfig(preset Preset, args []string, detach bool, entrypoint, name string,
	envFlags []string, cpu, memory string, duration time.Duration,
	shareProcessNamespace bool, runtimeClass string, gitURL, gitBranch string, gitDepth int32,
	gitSubpath, pvc string) *apiv1.CreateSandboxRequest {

	var containerCmd, containerArgs []string

	if detach && len(args) > 1 {
		if entrypoint != "" {
			containerCmd = []string{entrypoint}
		} else if len(preset.Command) > 0 {
			containerCmd = preset.Command
		}
		containerArgs = args[1:]
	} else {
		containerCmd = []string{"/bin/sh", "-c", "trap : TERM INT; sleep infinity & wait"}
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

	return req
}

func runExecutionSession(ctx context.Context, c *client.Client, id string, execCmd []string,
	ports []string, isTaskMode bool) error {

	_, _ = fmt.Fprintf(os.Stderr, "Waiting for sandbox %s to start...\n", id)

	waitCtx, waitCancel := context.WithTimeout(ctx, 3*time.Minute)
	defer waitCancel()

	readySbx, err := c.WaitForRunning(waitCtx, id, 1*time.Second)
	if err != nil {
		return fmt.Errorf("sandbox %s failed to start: %w", id, err)
	}

	cancelPortForwards := startPortForwarders(ctx, c, readySbx.ID, ports)
	defer cancelPortForwards()

	exitCode, execErr := executeSession(ctx, c, readySbx.ID, execCmd, isTaskMode)
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
