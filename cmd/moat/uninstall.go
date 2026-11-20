package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

type uninstallOptions struct {
	UserMode   bool
	Prefix     string
	DataDir    string
	ConfigDir  string
	UnitPath   string
	SkipUnit   bool
	KeepData   bool
	KeepConfig bool
	DryRun     bool
	Stdout     io.Writer
	RunCmd     func(name string, args ...string) error
}

func runUninstall(args []string) int {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	userMode := fs.Bool("user", os.Geteuid() != 0, "uninstall from $HOME paths instead of system paths")
	prefix := fs.String("prefix", "", "binary directory (default /usr/local/bin or ~/.local/bin)")
	dataDir := fs.String("data", "", "image and audit directory (default /var/lib/moat or ~/.local/share/moat)")
	cfgDir := fs.String("config", "", "config directory (default /etc/moat or ~/.config/moat)")
	keepData := fs.Bool("keep-data", false, "keep the data directory (images and audit logs)")
	keepConfig := fs.Bool("keep-config", false, "keep the config directory (moatd.json)")
	noService := fs.Bool("no-service", false, "do not touch the systemd unit")
	dryRun := fs.Bool("dry-run", false, "print the plan without removing files")
	_ = fs.Bool("yes", true, "accept removal without prompting")
	_ = fs.Parse(args)

	opts := resolveUninstallOptions(uninstallOptions{
		UserMode:   *userMode,
		Prefix:     *prefix,
		DataDir:    *dataDir,
		ConfigDir:  *cfgDir,
		SkipUnit:   *noService,
		KeepData:   *keepData,
		KeepConfig: *keepConfig,
		DryRun:     *dryRun,
		Stdout:     os.Stdout,
	})
	if err := executeUninstall(opts); err != nil {
		fmt.Fprintf(os.Stderr, "moat uninstall: %v\n", err)
		return 1
	}
	return 0
}

func resolveUninstallOptions(opts uninstallOptions) uninstallOptions {
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.RunCmd == nil {
		opts.RunCmd = func(name string, args ...string) error {
			return exec.Command(name, args...).Run()
		}
	}
	if opts.UserMode {
		home, _ := os.UserHomeDir()
		xdgData := os.Getenv("XDG_DATA_HOME")
		if xdgData == "" && home != "" {
			xdgData = filepath.Join(home, ".local", "share")
		}
		xdgCfg := os.Getenv("XDG_CONFIG_HOME")
		if xdgCfg == "" && home != "" {
			xdgCfg = filepath.Join(home, ".config")
		}
		if opts.Prefix == "" && home != "" {
			opts.Prefix = filepath.Join(home, ".local", "bin")
		}
		if opts.DataDir == "" && xdgData != "" {
			opts.DataDir = filepath.Join(xdgData, "moat")
		}
		if opts.ConfigDir == "" && xdgCfg != "" {
			opts.ConfigDir = filepath.Join(xdgCfg, "moat")
		}
		if opts.UnitPath == "" && xdgCfg != "" {
			opts.UnitPath = filepath.Join(xdgCfg, "systemd", "user", "moatd.service")
		}
	} else {
		if opts.Prefix == "" {
			opts.Prefix = "/usr/local/bin"
		}
		if opts.DataDir == "" {
			opts.DataDir = "/var/lib/moat"
		}
		if opts.ConfigDir == "" {
			opts.ConfigDir = "/etc/moat"
		}
		if opts.UnitPath == "" {
			opts.UnitPath = "/etc/systemd/system/moatd.service"
		}
	}
	return opts
}

var installedBinaries = []string{"moat", "moatd", "moat-mcp", "moat-uffd"}

func executeUninstall(opts uninstallOptions) error {
	out := opts.Stdout
	step := func(desc string, fn func() error) error {
		if opts.DryRun {
			fmt.Fprintf(out, "  [dry-run] %s\n", desc)
			return nil
		}
		fmt.Fprintf(out, "  %s\n", desc)
		return fn()
	}

	// Stop and disable the systemd unit if present.
	if !opts.SkipUnit && opts.UnitPath != "" {
		if _, err := os.Stat(opts.UnitPath); err == nil {
			scArgs := []string{"disable", "--now", "moatd.service"}
			reloadArgs := []string{"daemon-reload"}
			if opts.UserMode {
				scArgs = append([]string{"--user"}, scArgs...)
				reloadArgs = append([]string{"--user"}, reloadArgs...)
			}
			_ = step("stop and disable moatd.service", func() error {
				_ = opts.RunCmd("systemctl", scArgs...)
				return nil
			})
			if err := step("remove "+opts.UnitPath, func() error {
				return removeIfExists(opts.UnitPath)
			}); err != nil {
				return err
			}
			_ = step("reload systemd daemon", func() error {
				_ = opts.RunCmd("systemctl", reloadArgs...)
				return nil
			})
		}
	}

	// Remove installed binaries.
	if opts.Prefix != "" {
		for _, bin := range installedBinaries {
			p := filepath.Join(opts.Prefix, bin)
			if _, err := os.Stat(p); err != nil {
				continue
			}
			if err := step("remove "+p, func() error {
				return removeIfExists(p)
			}); err != nil {
				return err
			}
		}
	}

	// Remove config directory unless kept.
	if !opts.KeepConfig && opts.ConfigDir != "" {
		if _, err := os.Stat(opts.ConfigDir); err == nil {
			if err := step("remove "+opts.ConfigDir, func() error {
				return os.RemoveAll(opts.ConfigDir)
			}); err != nil {
				return err
			}
		}
	}

	// Remove data directory unless kept.
	if !opts.KeepData && opts.DataDir != "" {
		if _, err := os.Stat(opts.DataDir); err == nil {
			if err := step("remove "+opts.DataDir, func() error {
				return os.RemoveAll(opts.DataDir)
			}); err != nil {
				return err
			}
		}
	}

	if opts.DryRun {
		fmt.Fprintln(out, "moat uninstall: dry run complete")
	} else {
		fmt.Fprintln(out, "moat uninstalled")
	}
	return nil
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
