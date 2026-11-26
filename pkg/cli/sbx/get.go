package sbx

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	apiv1 "github.com/hoangndst/buddy-sandboxes/pkg/api/v1"
	"github.com/hoangndst/buddy-sandboxes/pkg/client"
)

func newGetCmd(opts ...*RootOptions) *cobra.Command {
	opt := resolveOptions(opts)
	var (
		flagSandbox   string
		flagUser      string
		flagWorkspace string
		flagTask      string
		flagOutput    string
	)

	cmd := &cobra.Command{
		Use:   "get <resource> [name]",
		Short: "Display one or many resources",
		Long: `Get displays resources in a tabular or JSON format.

Supported resource types:
  - sandboxes  (aliases: sandbox, sbx)
  - sessions   (aliases: session, sess)
  - secrets    (aliases: secret)
  - users      (aliases: user)
  - tasks      (aliases: task)
  - executions (aliases: execution, execs)
  - profiles   (aliases: profile, agent-profile, agent-profiles)`,
		Example: `  # List all sandboxes
  buddy-sbx get sandboxes

  # List sandboxes in short form
  buddy-sbx get sbx

  # List sandboxes with wide details
  buddy-sbx get sbx -o wide

  # Get a specific sandbox
  buddy-sbx get sbx my-box

  # List sandboxes belonging to a specific user
  buddy-sbx get sbx --user usr_alice

  # List sessions inside a sandbox
  buddy-sbx get sessions -s my-box

  # List credential secrets
  buddy-sbx get secrets

  # List tasks
  buddy-sbx get tasks

  # Get a specific task
  buddy-sbx get task task_01jfa3

  # List executions for a task
  buddy-sbx get executions --task task_01jfa3

  # List agent profiles
  buddy-sbx get profiles`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			defer func() {
				flagSandbox = ""
				flagUser = ""
				flagWorkspace = ""
				flagTask = ""
				flagOutput = ""
			}()

			resourceType := strings.ToLower(args[0])
			var resourceName string
			if len(args) == 2 {
				resourceName = args[1]
			}

			c, err := opt.GetClient()
			if err != nil {
				return err
			}

			switch resourceType {
			case resourceSandboxes, resourceSandbox, resourceSbx:
				return runGetSandboxes(cmd, c, resourceName, flagUser, flagOutput)
			case resourceSessions, resourceSession, resourceSess:
				var sbxID, sessID string
				if slashIdx := strings.IndexByte(resourceName, '/'); slashIdx >= 0 {
					sbxID = resourceName[:slashIdx]
					sessID = resourceName[slashIdx+1:]
				} else {
					sbxID = flagSandbox
					sessID = resourceName
					if sbxID == "" && sessID != "" {
						sbxID = sessID
						sessID = ""
					}
				}
				if sbxID == "" {
					return fmt.Errorf("session resource requires --sandbox or <sandbox-id>")
				}
				return runGetSessions(cmd, c, sbxID, sessID, flagOutput)
			case resourceSecrets, resourceSecret:
				return runGetSecrets(cmd, c, resourceName, flagOutput)
			case resourceUsers, resourceUser:
				return runGetUsers(cmd, c, resourceName, flagOutput)
			case resourceTasks, resourceTask:
				return runGetTasks(cmd, c, resourceName, flagWorkspace, flagOutput)
			case resourceExecutions, resourceExecution, resourceExecs:
				return runGetExecutions(cmd, c, flagTask, resourceName, flagOutput)
			case resourceProfiles, resourceProfile, resourceAgentProfile, resourceAgentProfiles:
				return runGetProfiles(cmd, c, resourceName, flagOutput)
			default:
				return fmt.Errorf("unknown resource type %q; "+
					"choose from: sandboxes (sbx), sessions (sess), secrets, users, tasks, executions, profiles", args[0])
			}
		},
	}

	cmd.Flags().StringVarP(&flagSandbox, "sandbox", "s", "", "Target sandbox for child resources (sessions)")
	cmd.Flags().StringVarP(&flagUser, "user", "u", "", "Filter sandboxes by user ID (admin only)")
	cmd.Flags().StringVarP(&flagWorkspace, "workspace", "w", "", "Filter tasks by workspace name")
	cmd.Flags().StringVarP(&flagTask, "task", "t", "", "Filter executions by task ID")
	cmd.Flags().StringVarP(&flagOutput, "output", "o", "", "Output format (json or wide)")

	return cmd
}

func runGetUserSandboxes(cmd *cobra.Command, c *client.Client, name, user, output string) error {
	sandboxes, err := c.ListUserSandboxes(cmd.Context(), user)
	if err != nil {
		return err
	}
	if name != "" {
		var filtered []apiv1.SandboxResponse
		for _, s := range sandboxes {
			if s.Name == name {
				filtered = append(filtered, s)
			}
		}
		if len(filtered) == 0 {
			return fmt.Errorf("sandbox %q not found", name)
		}
		sandboxes = filtered
	}
	if output == outputJSON {
		data, err := json.MarshalIndent(sandboxes, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return err
	}
	if len(sandboxes) == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "No active sandboxes found.")
		return err
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "NAME\tPHASE\tIMAGE\tNAMESPACE\tAGE"); err != nil {
		return err
	}
	now := time.Now()
	for _, s := range sandboxes {
		age := now.Sub(s.CreatedAt).Round(time.Second).String()
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			s.Name, s.Phase, s.Image, s.Namespace, age); err != nil {
			return err
		}
	}
	return w.Flush()
}

func runGetSandboxes(cmd *cobra.Command, c *client.Client, name, user, output string) error {
	if user != "" {
		return runGetUserSandboxes(cmd, c, name, user, output)
	}

	ctx := cmd.Context()

	resp, err := c.ListSandboxes(ctx)
	if err != nil {
		return err
	}

	items := resp.Items
	if name != "" {
		var matched []apiv1.SandboxResponse
		for _, sbx := range items {
			if sbx.Name == name {
				matched = append(matched, sbx)
			}
		}
		if len(matched) == 0 {
			return fmt.Errorf("sandbox %q not found", name)
		}
		items = matched
	}

	if output == outputJSON {
		data, err := json.MarshalIndent(items, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return err
	}

	if len(items) == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "No active sandboxes found.")
		return err
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 8, 2, ' ', 0)
	isWide := output == outputWide
	if isWide {
		_, _ = fmt.Fprintln(w, "NAME\tIMAGE\tPHASE\tAGE\tNETWORK\tPOD IP\tNAMESPACE\tMCP\tLABELS")
	} else {
		_, _ = fmt.Fprintln(w, "NAME\tIMAGE\tPHASE\tAGE\tNETWORK\tMCP\tLABELS")
	}

	now := time.Now()
	for _, sbx := range items {
		age := now.Sub(sbx.CreatedAt).Round(time.Second).String()
		netStr := "Web"
		if sbx.Network != nil {
			if len(sbx.Network.AllowedDomains) > 0 {
				netStr = fmt.Sprintf("Allowlist (%d)", len(sbx.Network.AllowedDomains))
			} else if !sbx.Network.AllowInternetWeb {
				netStr = "Isolated"
			}
		}
		var mcpParts []string
		for _, s := range sbx.MCPServers {
			mcpParts = append(mcpParts, fmt.Sprintf("%s:%d", s.Name, s.Port))
		}
		mcpStr := strings.Join(mcpParts, ",")
		if mcpStr == "" {
			mcpStr = "-"
		}
		var labelParts []string
		for k, v := range sbx.Labels {
			labelParts = append(labelParts, fmt.Sprintf("%s=%s", k, v))
		}
		labelStr := strings.Join(labelParts, ",")
		if labelStr == "" {
			labelStr = "-"
		}

		if isWide {
			podIP := sbx.PodIP
			if podIP == "" {
				podIP = "-"
			}
			ns := sbx.Namespace
			if ns == "" {
				ns = "-"
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				sbx.Name, sbx.Image, sbx.Phase, age, netStr, podIP, ns, mcpStr, labelStr)
		} else {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				sbx.Name, sbx.Image, sbx.Phase, age, netStr, mcpStr, labelStr)
		}
	}
	return w.Flush()
}

func runGetSessions(cmd *cobra.Command, c *client.Client, sandboxID, sessionID, output string) error {
	resp, err := c.ListSessions(cmd.Context(), sandboxID)
	if err != nil {
		return err
	}

	sessions := resp.Sessions
	if sessionID != "" {
		var matched []apiv1.SessionResponse
		for _, s := range sessions {
			if s.ID == sessionID {
				matched = append(matched, s)
			}
		}
		if len(matched) == 0 {
			return fmt.Errorf("session %q not found in sandbox %q", sessionID, sandboxID)
		}
		sessions = matched
	}

	if output == outputJSON {
		data, err := json.MarshalIndent(sessions, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return err
	}

	if len(sessions) == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "No active or historical sessions found.")
		return err
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 8, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "SESSION ID\tSTATE\tEXIT CODE\tCOMMAND\tCREATED")
	for _, s := range sessions {
		exitCodeStr := "-"
		if s.ExitCode != nil {
			exitCodeStr = fmt.Sprintf("%d", *s.ExitCode)
		}
		cmdStr := strings.Join(s.Command, " ")
		if len(cmdStr) > 40 {
			cmdStr = cmdStr[:37] + "..."
		}
		createdStr := s.CreatedAt.Format(time.RFC3339)
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.ID, s.State, exitCodeStr, cmdStr, createdStr)
	}
	return w.Flush()
}

func runGetSecrets(cmd *cobra.Command, c *client.Client, name, output string) error {
	items, err := c.ListCredentials(cmd.Context())
	if err != nil {
		return err
	}

	if name != "" {
		var matched []apiv1.CredentialMetadata
		for _, item := range items {
			if strings.EqualFold(item.Service, name) {
				matched = append(matched, item)
			}
		}
		if len(matched) == 0 {
			return fmt.Errorf("secret %q not found", name)
		}
		items = matched
	}

	if output == outputJSON {
		data, err := json.MarshalIndent(items, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return err
	}

	if output == outputWide {
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
		headers := "SERVICE\tSCOPE\tSANDBOX\tREPOSITORY\tBASE_URL\tMODEL\tRPM\tCONCURRENCY\tTPM\tUPDATED"
		if _, err := fmt.Fprintln(w, headers); err != nil {
			return err
		}
		for _, item := range items {
			updated := item.UpdatedAt.Local().Format("2006-01-02 15:04:05 MST")
			bURL := item.BaseURL
			if bURL == "" {
				bURL = "-"
			}
			model := item.DefaultModel
			if model == "" {
				model = "-"
			}
			repo := item.Repository
			if repo == "" {
				repo = "-"
			}
			rpm := "-"
			if item.RequestsPerMinute > 0 {
				rpm = strconv.Itoa(item.RequestsPerMinute)
			}
			conc := "-"
			if item.MaxConcurrent > 0 {
				conc = strconv.Itoa(item.MaxConcurrent)
			}
			tpm := "-"
			if item.TokensPerMinute > 0 {
				tpm = strconv.FormatInt(item.TokensPerMinute, 10)
			}
			if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				item.Service, item.Scope, item.SandboxName, repo, bURL, model, rpm, conc, tpm, updated); err != nil {
				return err
			}
		}
		return w.Flush()
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "SERVICE\tSCOPE\tSANDBOX\tREPOSITORY\tBASE_URL\tMODEL\tUPDATED"); err != nil {
		return err
	}
	for _, item := range items {
		updated := item.UpdatedAt.Local().Format("2006-01-02 15:04:05 MST")
		bURL := item.BaseURL
		if bURL == "" {
			bURL = "-"
		}
		model := item.DefaultModel
		if model == "" {
			model = "-"
		}
		repo := item.Repository
		if repo == "" {
			repo = "-"
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			item.Service, item.Scope, item.SandboxName, repo, bURL, model, updated); err != nil {
			return err
		}
	}
	return w.Flush()
}

func runGetUsers(cmd *cobra.Command, c *client.Client, name, output string) error {
	users, err := c.ListUsers(cmd.Context())
	if err != nil {
		return err
	}

	if name != "" {
		var matched []apiv1.User
		for _, u := range users {
			if u.ID == name || strings.EqualFold(u.DisplayName, name) {
				matched = append(matched, u)
			}
		}
		if len(matched) == 0 {
			return fmt.Errorf("user %q not found", name)
		}
		users = matched
	}

	if output == outputJSON {
		data, err := json.MarshalIndent(users, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return err
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "ID\tDISPLAY NAME\tNAMESPACE\tROLE\tSTATUS\tCREATED"); err != nil {
		return err
	}
	for _, u := range users {
		status := "active"
		if u.Disabled {
			status = "disabled"
		}
		created := u.Created.Local().Format("2006-01-02 15:04:05 MST")
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			u.ID, u.DisplayName, u.Namespace, u.Role, status, created); err != nil {
			return err
		}
	}
	return w.Flush()
}

func runGetTasks(cmd *cobra.Command, c *client.Client, taskID, workspace, output string) error {
	if taskID != "" {
		t, err := c.GetTask(cmd.Context(), taskID)
		if err != nil {
			return err
		}
		if output == outputJSON {
			data, err := json.MarshalIndent(t, "", "  ")
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
			return err
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 8, 2, ' ', 0)
		isWide := output == outputWide
		if isWide {
			_, _ = fmt.Fprintln(w, "TASK ID\tSTATUS\tWORKSPACE\tPROMPT\tACTIVE EXECUTION\tPROFILE\tNAMESPACE\tCREATED")
		} else {
			_, _ = fmt.Fprintln(w, "TASK ID\tSTATUS\tWORKSPACE\tPROMPT\tACTIVE EXECUTION\tCREATED")
		}
		promptStr := t.Prompt
		if len(promptStr) > 40 {
			promptStr = promptStr[:37] + "..."
		}
		activeExec := "-"
		if t.ActiveExecutionID != nil && *t.ActiveExecutionID != "" {
			activeExec = *t.ActiveExecutionID
		}
		createdStr := t.CreatedAt.Local().Format("2006-01-02 15:04:05")
		if isWide {
			profStr := "-"
			if t.AgentProfileID != nil && *t.AgentProfileID != "" {
				profStr = *t.AgentProfileID
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				t.ID, t.Status, t.WorkspaceName, promptStr, activeExec, profStr, t.Namespace, createdStr)
		} else {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				t.ID, t.Status, t.WorkspaceName, promptStr, activeExec, createdStr)
		}
		return w.Flush()
	}

	resp, err := c.ListTasks(cmd.Context(), "", workspace, "")
	if err != nil {
		return err
	}
	tasks := resp.Tasks
	if output == outputJSON {
		data, err := json.MarshalIndent(tasks, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return err
	}
	if len(tasks) == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "No tasks found.")
		return err
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 8, 2, ' ', 0)
	isWide := output == outputWide
	if isWide {
		_, _ = fmt.Fprintln(w, "TASK ID\tSTATUS\tWORKSPACE\tPROMPT\tACTIVE EXECUTION\tPROFILE\tNAMESPACE\tCREATED")
	} else {
		_, _ = fmt.Fprintln(w, "TASK ID\tSTATUS\tWORKSPACE\tPROMPT\tACTIVE EXECUTION\tCREATED")
	}
	for _, t := range tasks {
		promptStr := t.Prompt
		if len(promptStr) > 40 {
			promptStr = promptStr[:37] + "..."
		}
		activeExec := "-"
		if t.ActiveExecutionID != nil && *t.ActiveExecutionID != "" {
			activeExec = *t.ActiveExecutionID
		}
		createdStr := t.CreatedAt.Local().Format("2006-01-02 15:04:05")
		if isWide {
			profStr := "-"
			if t.AgentProfileID != nil && *t.AgentProfileID != "" {
				profStr = *t.AgentProfileID
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				t.ID, t.Status, t.WorkspaceName, promptStr, activeExec, profStr, t.Namespace, createdStr)
		} else {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				t.ID, t.Status, t.WorkspaceName, promptStr, activeExec, createdStr)
		}
	}
	return w.Flush()
}

func runGetExecutions(cmd *cobra.Command, c *client.Client, taskID, executionID, output string) error {
	isWide := output == outputWide
	if executionID != "" {
		e, err := c.GetExecution(cmd.Context(), executionID)
		if err != nil {
			return err
		}
		if output == outputJSON {
			data, err := json.MarshalIndent(e, "", "  ")
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
			return err
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 8, 2, ' ', 0)
		if isWide {
			_, _ = fmt.Fprintln(w, "EXECUTION ID\tTASK ID\tATTEMPT\tSTATUS\tWORKSPACE\tEXIT CODE\tVERIFIED\tRUNNER\tCREATED")
		} else {
			_, _ = fmt.Fprintln(w, "EXECUTION ID\tTASK ID\tATTEMPT\tSTATUS\tEXIT CODE\tVERIFIED\tCREATED")
		}
		exitStr := "-"
		if e.ExitCode != nil {
			exitStr = fmt.Sprintf("%d", *e.ExitCode)
		}
		verifiedStr := "-"
		if e.VerificationPassed != nil {
			verifiedStr = strconv.FormatBool(*e.VerificationPassed)
		}
		createdStr := e.CreatedAt.Local().Format("2006-01-02 15:04:05")
		if isWide {
			runner := "-"
			if e.SandboxName != nil && *e.SandboxName != "" {
				runner = *e.SandboxName
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
				e.ID, e.TaskID, e.AttemptNumber, e.Status, e.WorkspaceName, exitStr, verifiedStr, runner, createdStr)
		} else {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
				e.ID, e.TaskID, e.AttemptNumber, e.Status, exitStr, verifiedStr, createdStr)
		}
		return w.Flush()
	}

	if taskID == "" {
		return fmt.Errorf("executions resource requires --task <task-id> or <execution-id>")
	}

	resp, err := c.ListExecutions(cmd.Context(), taskID)
	if err != nil {
		return err
	}
	execs := resp.Executions
	if output == outputJSON {
		data, err := json.MarshalIndent(execs, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return err
	}
	if len(execs) == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "No executions found for task.")
		return err
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 8, 2, ' ', 0)
	if isWide {
		_, _ = fmt.Fprintln(w, "EXECUTION ID\tTASK ID\tATTEMPT\tSTATUS\tWORKSPACE\tEXIT CODE\tVERIFIED\tRUNNER\tCREATED")
	} else {
		_, _ = fmt.Fprintln(w, "EXECUTION ID\tTASK ID\tATTEMPT\tSTATUS\tEXIT CODE\tVERIFIED\tCREATED")
	}
	for _, e := range execs {
		exitStr := "-"
		if e.ExitCode != nil {
			exitStr = fmt.Sprintf("%d", *e.ExitCode)
		}
		verifiedStr := "-"
		if e.VerificationPassed != nil {
			verifiedStr = strconv.FormatBool(*e.VerificationPassed)
		}
		createdStr := e.CreatedAt.Local().Format("2006-01-02 15:04:05")
		if isWide {
			runner := "-"
			if e.SandboxName != nil && *e.SandboxName != "" {
				runner = *e.SandboxName
			}
			_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
				e.ID, e.TaskID, e.AttemptNumber, e.Status, e.WorkspaceName, exitStr, verifiedStr, runner, createdStr)
		} else {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n",
				e.ID, e.TaskID, e.AttemptNumber, e.Status, exitStr, verifiedStr, createdStr)
		}
	}
	return w.Flush()
}

func runGetProfiles(cmd *cobra.Command, c *client.Client, profileID, output string) error {
	isWide := output == outputWide
	if profileID != "" {
		p, err := c.GetAgentProfile(cmd.Context(), profileID)
		if err != nil {
			return err
		}
		if output == outputJSON {
			data, err := json.MarshalIndent(p, "", "  ")
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
			return err
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 8, 2, ' ', 0)
		if isWide {
			_, _ = fmt.Fprintln(w, "PROFILE ID\tNAME\tHARNESS\tMODEL\tSTEPS\tTIMEOUT\tVERIFY COMMAND")
		} else {
			_, _ = fmt.Fprintln(w, "PROFILE ID\tNAME\tHARNESS\tMODEL\tVERIFY COMMAND")
		}
		verifyCmd := "-"
		if p.VerificationCommand != "" {
			verifyCmd = p.VerificationCommand
		}
		if isWide {
			timeoutStr := fmt.Sprintf("%ds", p.TimeoutSeconds)
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
				p.ID, p.Name, p.HarnessType, p.ModelName, p.MaxSteps, timeoutStr, verifyCmd)
		} else {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				p.ID, p.Name, p.HarnessType, p.ModelName, verifyCmd)
		}
		return w.Flush()
	}

	resp, err := c.ListAgentProfiles(cmd.Context())
	if err != nil {
		return err
	}
	profiles := resp.Profiles
	if output == outputJSON {
		data, err := json.MarshalIndent(profiles, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return err
	}
	if len(profiles) == 0 {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), "No agent profiles found.")
		return err
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 8, 2, ' ', 0)
	if isWide {
		_, _ = fmt.Fprintln(w, "PROFILE ID\tNAME\tHARNESS\tMODEL\tSTEPS\tTIMEOUT\tVERIFY COMMAND")
	} else {
		_, _ = fmt.Fprintln(w, "PROFILE ID\tNAME\tHARNESS\tMODEL\tVERIFY COMMAND")
	}
	for _, p := range profiles {
		verifyCmd := "-"
		if p.VerificationCommand != "" {
			verifyCmd = p.VerificationCommand
		}
		if isWide {
			timeoutStr := fmt.Sprintf("%ds", p.TimeoutSeconds)
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
				p.ID, p.Name, p.HarnessType, p.ModelName, p.MaxSteps, timeoutStr, verifyCmd)
		} else {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				p.ID, p.Name, p.HarnessType, p.ModelName, verifyCmd)
		}
	}
	return w.Flush()
}
