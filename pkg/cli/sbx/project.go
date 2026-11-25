package sbx

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/hoangndst/buddy-sandboxes/internal/workspace"
	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
	"github.com/hoangndst/buddy-sandboxes/pkg/client"
	"golang.org/x/term"
)

type projectRunOptions struct {
	Path          string
	TaskArgs      []string
	Preview       bool
	Detach        bool
	Remove        bool
	NoSync        bool
	Duration      time.Duration
	Name          string
	IdleCommand   string
	IdleArgs      []string
	EntryPoint    string
	Ports         []string
	EnvFlags      []string
	Preset        Preset
	CreateRequest *apiv1.CreateSandboxRequest
	AgentCommand  []string
}

const (
	openAIAPIKeyEnv    = "OPENAI_API_KEY"
	anthropicAPIKeyEnv = "ANTHROPIC_API_KEY"
	providerModeEnv    = "BUDDY_PROVIDER_MODE"
	providerOpenAI     = "openai"
	providerAnthropic  = "anthropic"
)

func splitRunArguments(args []string, dashAt int) (target, projectPath string, taskArgs []string, err error) {
	front := args
	if dashAt >= 0 {
		if dashAt > len(args) {
			return "", "", nil, fmt.Errorf("invalid command separator position")
		}
		front = args[:dashAt]
		taskArgs = append([]string(nil), args[dashAt:]...)
	}
	if len(front) == 0 {
		return "", "", nil, fmt.Errorf("an agent preset or image is required")
	}
	if len(front) > 2 {
		return "", "", nil, fmt.Errorf("task arguments must follow --")
	}
	target = front[0]
	if len(front) == 2 {
		projectPath = front[1]
	}
	return target, projectPath, taskArgs, nil
}

//nolint:gocyclo // This coordinates the project snapshot, session, execution, sync, and cleanup lifecycle.
func runProjectWorkflow(ctx context.Context, c *client.Client, opts projectRunOptions) error {
	hasProject := opts.Path != ""
	if opts.Detach && opts.Preview {
		return fmt.Errorf("--preview cannot be combined with --detach")
	}
	if !hasProject && !opts.Detach {
		return fmt.Errorf("local project directory is required")
	}
	isCodex := strings.EqualFold(opts.Preset.Name, "codex")
	isClaude := strings.EqualFold(opts.Preset.Name, "claude")
	usesProvider := isCodex || isClaude
	if hasProject && usesProvider && opts.Duration > 2*time.Hour {
		return fmt.Errorf("provider-backed agent runs cannot exceed a two-hour deadline")
	}
	if hasProject && usesProvider && opts.Duration == 0 {
		opts.Duration = 2 * time.Hour
	}
	if opts.Detach {
		if len(opts.TaskArgs) != 0 || opts.EntryPoint != "" {
			return fmt.Errorf("--detach does not accept task arguments or --entrypoint")
		}
		if opts.Remove {
			return fmt.Errorf("--rm cannot be used with --detach")
		}
		if !isPinnedPreset(opts.Preset) && opts.IdleCommand == "" {
			return fmt.Errorf("detaching a custom image requires --idle-command")
		}
	}
	for _, item := range opts.EnvFlags {
		name := item
		if i := strings.IndexByte(item, '='); i >= 0 {
			name = item[:i]
		}
		if name == openAIAPIKeyEnv || name == anthropicAPIKeyEnv || name == providerModeEnv {
			return fmt.Errorf("%s is managed by Buddy and cannot be set with --env", name)
		}
	}

	var archivePath string
	var baseline workspace.Baseline
	var snapshotStats workspace.Stats
	if hasProject {
		temp, err := os.CreateTemp("", "buddy-sbx-project-*.tar")
		if err != nil {
			return fmt.Errorf("create project snapshot: %w", err)
		}
		archivePath = temp.Name()
		defer func() { _ = os.Remove(archivePath) }()
		baseline, snapshotStats, err = workspace.Pack(ctx, opts.Path, temp, workspace.DefaultLimits())
		closeErr := temp.Close()
		if err != nil {
			return fmt.Errorf("pack project snapshot: %w", err)
		}
		if closeErr != nil {
			return fmt.Errorf("close project snapshot: %w", closeErr)
		}
		if snapshotStats.Files == 0 {
			return fmt.Errorf("project snapshot contains no files after exclusions")
		}
		if opts.Preview {
			printProjectPreview(opts.Path, baseline, snapshotStats)
			return nil
		}
		printSnapshotSummary(snapshotStats)
	} else if opts.Preview {
		return fmt.Errorf("--preview requires a local project directory")
	}

	creationID, err := randomURLToken(16)
	if err != nil {
		return err
	}
	name := opts.Name
	if name == "" {
		name = generatedNameFromID(creationID)
	}
	if _, err := c.GetSandbox(ctx, name); err == nil {
		return fmt.Errorf("sandbox %q already exists", name)
	} else if !isHTTPStatus(err, 404) {
		return fmt.Errorf("check sandbox name %q: %w", name, err)
	}

	createReq := *opts.CreateRequest
	createReq.Name = name
	createReq.CreationID = creationID
	createReq.WorkingDir = "/workspace"
	if opts.Duration > 0 {
		seconds := int64(opts.Duration / time.Second)
		if opts.Duration%time.Second != 0 {
			seconds++
		}
		createReq.MaxDurationSeconds = &seconds
	}
	if hasProject {
		createReq.Workspace = &apiv1.WorkspaceSpec{Ephemeral: true, MountPath: "/workspace"}
	}
	if usesProvider {
		if isCodex {
			createReq.Provider = providerOpenAI
		} else {
			createReq.Provider = providerAnthropic
		}
		if createReq.Network == nil {
			createReq.Network = &apiv1.NetworkSpec{
				AllowInternetWeb: true, AllowDNS: true, BlockClusterInternal: true, BlockMetadataService: true,
			}
		}
		createReq.Network.AllowAPIBroker = true
	}
	if opts.Detach {
		if isPinnedPreset(opts.Preset) {
			createReq.Command = []string{"/bin/sh", "-c", "sleep infinity"}
		} else {
			createReq.Command = []string{opts.IdleCommand}
			createReq.Args = opts.IdleArgs
		}
	} else {
		createReq.Command = []string{
			"/bin/sh", "-c", "trap 'exit 0' TERM INT; while :; do sleep 3600 & wait $!; done",
		}
	}

	sandbox, err := c.CreateSandbox(ctx, &createReq)
	if err != nil {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		var cleanupErrs []error
		if existing, getErr := c.GetSandbox(cleanupCtx, name); getErr == nil {
			if existing.CreationID == creationID {
				if deleteErr := c.DeleteSandbox(cleanupCtx, name); deleteErr != nil {
					cleanupErrs = append(cleanupErrs, fmt.Errorf("delete partially created Sandbox %q: %w", name, deleteErr))
				}
			}
		} else if !isHTTPStatus(getErr, 404) {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("check partially created Sandbox %q: %w", name, getErr))
		}
		return errors.Join(append([]error{fmt.Errorf("create sandbox %q: %w", name, err)}, cleanupErrs...)...)
	}
	owned := true
	cleanupSetup := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var cleanupErrs []error
		if owned {
			if deleteErr := c.DeleteSandbox(cleanupCtx, sandbox.ID); deleteErr != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("delete Sandbox %q: %w", sandbox.ID, deleteErr))
			}
		}
		return errors.Join(cleanupErrs...)
	}
	waitCtx, waitCancel := context.WithTimeout(ctx, 3*time.Minute)
	ready, err := c.WaitForRunning(waitCtx, sandbox.ID, time.Second)
	waitCancel()
	if err != nil {
		return errors.Join(fmt.Errorf("sandbox setup failed for Sandbox %q: %w", sandbox.ID, err), cleanupSetup())
	}

	if hasProject {
		archive, err := os.Open(archivePath)
		if err != nil {
			return errors.Join(fmt.Errorf("open project snapshot: %w", err), cleanupSetup())
		}
		err = c.Upload(ctx, ready.ID, "/workspace", archive)
		_ = archive.Close()
		if err != nil {
			return errors.Join(fmt.Errorf("upload project snapshot to Sandbox %q: %w", sandbox.ID, err), cleanupSetup())
		}
		if !strings.EqualFold(opts.Preset.Name, "shell") {
			if err := initializeWorkspace(ctx, c, ready.ID); err != nil {
				return errors.Join(fmt.Errorf("initialize project workspace in Sandbox %q: %w", ready.ID, err), cleanupSetup())
			}
		}
	}

	if opts.Detach {
		owned = false
		fmt.Println(ready.ID)
		return nil
	}

	cancelPortForwards := startPortForwarders(ctx, c, ready.ID, opts.Ports)
	exitCode, execErr := executeSession(ctx, c, ready.ID, opts.AgentCommand, len(opts.TaskArgs) > 0)
	cancelPortForwards()

	if ctx.Err() != nil || errors.Is(execErr, context.Canceled) {
		_, _ = fmt.Fprintln(os.Stderr, "⚠ Execution cancelled by user. Local workspace untouched.")
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return execErr
	}
	if execErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "✗ Agent execution failed: %v. Local workspace untouched.\n", execErr)
		return execErr
	}
	if exitCode != 0 && len(opts.TaskArgs) > 0 {
		_, _ = fmt.Fprintf(os.Stderr,
			"✗ Agent exited with code %d. Local workspace untouched. Sandbox %s retained for recovery.\n",
			exitCode, ready.ID)
		return ExitCodeError{Code: exitCode}
	}

	if opts.NoSync {
		_, _ = fmt.Fprintln(os.Stderr, "ℹ Workspace sync skipped (--no-sync): local project directory untouched.")
		if opts.Remove {
			if err := deleteSandboxAfterSync(ctx, c, ready.ID); err != nil {
				return fmt.Errorf("sandbox %s deletion failed: %w", ready.ID, err)
			}
			owned = false
		}
		if exitCode != 0 {
			return ExitCodeError{Code: exitCode}
		}
		return nil
	}

	mergeCtx, mergeCancel := context.WithTimeout(context.Background(), workspace.DefaultLimits().TransferTimeout)
	remoteArchive, downloadErr := c.DownloadWorkspace(mergeCtx, ready.ID)
	if downloadErr != nil {
		mergeCancel()
		return fmt.Errorf("download project results failed; sandbox %s retained for recovery: %w", ready.ID, downloadErr)
	}
	merge, mergeErr := workspace.MergeArchive(
		mergeCtx, opts.Path, name, baseline, remoteArchive, workspace.DefaultLimits(),
	)
	_ = remoteArchive.Close()
	mergeCancel()
	if mergeErr != nil {
		return fmt.Errorf("merge project results failed; sandbox %s retained for recovery: %w", ready.ID, mergeErr)
	}
	if len(merge.Conflicts) > 0 {
		for _, conflict := range merge.Conflicts {
			_, _ = fmt.Fprintf(os.Stderr, "conflict %s: %s\n", conflict.Path, conflict.Reason)
		}
		return fmt.Errorf("project changes conflict with local edits; sandbox %s retained", ready.ID)
	}

	printMergeSummary(os.Stderr, merge)

	if opts.Remove {
		if err := deleteSandboxAfterSync(ctx, c, ready.ID); err != nil {
			return fmt.Errorf("workspace synchronized, but sandbox %s deletion failed: %w", ready.ID, err)
		}
		owned = false
	}
	if exitCode != 0 {
		return ExitCodeError{Code: exitCode}
	}
	return nil
}

func printMergeSummary(w io.Writer, merge workspace.MergeResult) {
	totalChanges := len(merge.Added) + len(merge.Modified) + len(merge.Deleted)
	if totalChanges == 0 {
		_, _ = fmt.Fprintln(w, "✓ Workspace synchronized (no files changed).")
		return
	}

	_, _ = fmt.Fprintln(w, "✓ Workspace synchronized:")

	type fileChange struct {
		status string
		path   string
	}
	changes := make([]fileChange, 0, totalChanges)
	for _, p := range merge.Modified {
		changes = append(changes, fileChange{status: "M", path: p})
	}
	for _, p := range merge.Added {
		changes = append(changes, fileChange{status: "A", path: p})
	}
	for _, p := range merge.Deleted {
		changes = append(changes, fileChange{status: "D", path: p})
	}

	sort.Slice(changes, func(i, j int) bool {
		return changes[i].path < changes[j].path
	})

	const maxDisplay = 20
	limit := len(changes)
	if limit > maxDisplay {
		limit = maxDisplay
	}

	for i := 0; i < limit; i++ {
		_, _ = fmt.Fprintf(w, "  %s %s\n", changes[i].status, changes[i].path)
	}

	if len(changes) > maxDisplay {
		_, _ = fmt.Fprintf(w, "  ... and %d more files\n", len(changes)-maxDisplay)
	}
}

func deleteSandboxAfterSync(parent context.Context, c *client.Client, id string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 20*time.Second)
	defer cancel()
	return c.DeleteSandbox(ctx, id)
}

func executeSession(ctx context.Context, c *client.Client, id string, command []string, isTaskMode bool) (int, error) {
	if isTaskMode {
		var stdin io.Reader
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			stdin = os.Stdin
		}
		return c.Exec(ctx, id, client.ExecOptions{
			Cmd:        command,
			TTY:        false,
			Stdin:      stdin,
			Stdout:     os.Stdout,
			Stderr:     os.Stderr,
			ResizeChan: nil,
		})
	}

	isTTY := term.IsTerminal(int(os.Stdin.Fd()))
	var resizeChan chan apiv1.ResizeMessage
	if isTTY {
		resize, cleanup, err := setupTTY()
		if err == nil {
			defer cleanup()
			resizeChan = resize
		}
	}
	return c.Exec(ctx, id, client.ExecOptions{
		Cmd: command, TTY: isTTY, Stdin: os.Stdin, Stdout: os.Stdout,
		Stderr: os.Stderr, ResizeChan: resizeChan,
	})
}

func initializeWorkspace(ctx context.Context, c *client.Client, id string) error {
	cmd := []string{
		"/bin/sh", "-c",
		"git config --global --add safe.directory /workspace && git init -q && " +
			"git config user.name 'Buddy Sandbox' && " +
			"git config user.email 'buddy-sandbox@localhost' && git add -A && " +
			"git -c user.name='Buddy Sandbox' -c user.email='buddy-sandbox@localhost' " +
			"commit --allow-empty -qm 'Initial workspace snapshot'",
	}
	exit, err := c.Exec(ctx, id, client.ExecOptions{Cmd: cmd, Stdout: io.Discard, Stderr: io.Discard})
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("git initialization exited with code %d", exit)
	}
	return nil
}

func printProjectPreview(root string, baseline workspace.Baseline, stats workspace.Stats) {
	paths := make([]string, 0, len(baseline))
	for name := range baseline {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	fmt.Printf(
		"Project: %s\nIncluded entries: %d\nFiles: %d\nData: %d bytes\n",
		root, stats.Entries, stats.Files, stats.TotalBytes,
	)
	fmt.Println("Included paths:")
	for _, name := range paths {
		fmt.Printf("  %s\n", name)
	}
	if len(stats.ExcludedPath) > 0 {
		fmt.Println("Excluded paths:")
		for _, name := range stats.ExcludedPath {
			fmt.Printf("  %s\n", name)
		}
	}
}

func printSnapshotSummary(stats workspace.Stats) {
	_, _ = fmt.Fprintf(
		os.Stderr, "Project snapshot: %d files, %d entries, %d bytes\n",
		stats.Files, stats.Entries, stats.TotalBytes,
	)
	if len(stats.ExcludedPath) == 0 {
		_, _ = fmt.Fprintln(os.Stderr, "Excluded paths: none")
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, "Excluded paths (%d):\n", len(stats.ExcludedPath))
	for _, path := range stats.ExcludedPath {
		_, _ = fmt.Fprintf(os.Stderr, "  %s\n", path)
	}
}

func randomURLToken(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate session identifier: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func generatedNameFromID(id string) string {
	data, _ := base64.RawURLEncoding.DecodeString(id)
	return "buddy-" + hex.EncodeToString(data[:5])
}

func isPinnedPreset(preset Preset) bool {
	return preset.Name == "codex" || preset.Name == "claude" || preset.Name == "shell"
}

func isHTTPStatus(err error, code int) bool {
	return err != nil && strings.Contains(err.Error(), fmt.Sprintf("status %d", code))
}
