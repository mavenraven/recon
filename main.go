package main

import (
	"fmt"
	"os"

	"grecon/client"
	"grecon/server"

	"github.com/spf13/cobra"
)

func main() {
	rootCmd := &cobra.Command{
		Use:     "grecon",
		Short:   "Monitor and manage Claude Code sessions running in tmux",
		Version: "0.6.1",
		RunE: func(cmd *cobra.Command, args []string) error {
			return client.RunTUI()
		},
		SilenceUsage: true,
	}

	newCmd := &cobra.Command{
		Use:   "new [session-name]",
		Short: "Interactive form to create a new tmux session",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var initialName string
			if len(args) > 0 {
				initialName = args[0]
			}
			name, ok := client.RunNewSessionForm(initialName)
			if ok && name != "" {
				client.SwitchToPane(name)
			}
			return nil
		},
	}

	var launchName, launchCWD, launchCommand string
	var launchAttach, launchWorktree bool
	var launchTags []string
	launchCmd := &cobra.Command{
		Use:   "launch",
		Short: "Create a new claude session (background by default)",
		RunE: func(cmd *cobra.Command, args []string) error {
			defName, defCWD := client.DefaultNewSessionInfo()
			name := defName
			if launchName != "" {
				name = launchName
			}
			cwd := defCWD
			if launchCWD != "" {
				cwd = launchCWD
			}
			var cmdPtr *string
			if launchCommand != "" {
				cmdPtr = &launchCommand
			}
			claudeName := client.GenerateFunName()
			sessName, err := client.CreateSession(name, cwd, claudeName, cmdPtr, launchTags, launchWorktree)
			if err != nil {
				return err
			}
			if launchAttach {
				client.SwitchToPane(sessName)
			}
			fmt.Fprintf(os.Stderr, "Session: %s\n", sessName)
			return nil
		},
	}
	launchCmd.Flags().StringVar(&launchName, "name", "", "Custom session name")
	launchCmd.Flags().StringVar(&launchCWD, "cwd", "", "Working directory")
	launchCmd.Flags().StringVar(&launchCommand, "command", "", "Custom command to run")
	launchCmd.Flags().BoolVar(&launchAttach, "attach", false, "Attach after creating")
	launchCmd.Flags().StringSliceVar(&launchTags, "tag", nil, "Tag the session (key:value)")
	launchCmd.Flags().BoolVar(&launchWorktree, "worktree", false, "Create a git worktree")

	var resumeID, resumeName string
	var resumeNoAttach bool
	resumeCmd := &cobra.Command{
		Use:   "resume",
		Short: "Resume a past session (interactive picker, or by ID)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if resumeID != "" {
				var namePtr *string
				if resumeName != "" {
					namePtr = &resumeName
				}
				sess, err := client.ResumeSession(resumeID, namePtr)
				if err != nil {
					return err
				}
				if !resumeNoAttach {
					client.SwitchToPane(sess)
				}
				fmt.Fprintf(os.Stderr, "Resumed in session: %s\n", sess)
				return nil
			}
			sessionID, sessName, ok := client.RunResumePicker()
			if !ok {
				return nil
			}
			sess, err := client.ResumeSession(sessionID, &sessName)
			if err != nil {
				return err
			}
			client.SwitchToPane(sess)
			fmt.Fprintf(os.Stderr, "Resumed in session: %s\n", sess)
			return nil
		},
	}
	resumeCmd.Flags().StringVar(&resumeID, "id", "", "Session ID to resume directly")
	resumeCmd.Flags().StringVar(&resumeName, "name", "", "Custom tmux session name")
	resumeCmd.Flags().BoolVar(&resumeNoAttach, "no-attach", false, "Don't attach after resuming")

	nextCmd := &cobra.Command{
		Use:   "next",
		Short: "Jump to the next agent waiting for input",
		RunE: func(cmd *cobra.Command, args []string) error {
			app := client.NewApp()
			if err := app.Refresh(); err != nil {
				return err
			}
			for _, s := range app.Sessions {
				if s.Status == server.StatusInput && s.PaneTarget != "" {
					client.SwitchToPane(s.PaneTarget)
					return nil
				}
			}
			return nil
		},
	}

	serverCmd := &cobra.Command{
		Use:   "server",
		Short: "Run a background server that caches session data",
		Run: func(cmd *cobra.Command, args []string) {
			server.RunServer()
		},
	}

	rootCmd.AddCommand(newCmd, launchCmd, resumeCmd, nextCmd, serverCmd)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
