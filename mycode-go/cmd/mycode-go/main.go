package main

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
)

const cliName = "mycode-go"

func main() {
	os.Exit(runMain(os.Args[1:]))
}

func runMain(args []string) int {
	if len(args) == 0 {
		printUsage(os.Stderr)
		return 2
	}

	switch args[0] {
	case "-h", "--help", "help":
		printUsage(os.Stdout)
		return 0
	case "-V", "--version", "version":
		_, _ = fmt.Fprintf(os.Stdout, "%s %s\n", cliName, cliVersion())
		return 0
	case "run":
		return runCommand(args[1:])
	case "web":
		return webCommand(args[1:])
	case "session":
		return sessionCommand(args[1:])
	default:
		if strings.HasPrefix(args[0], "-") {
			printUsage(os.Stderr)
			return 2
		}
		return runCommand(args)
	}
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "Usage:")
	_, _ = fmt.Fprintf(w, "  %s run [flags] <message>\n", cliName)
	_, _ = fmt.Fprintf(w, "  %s web [flags]\n", cliName)
	_, _ = fmt.Fprintf(w, "  %s session list [--all]\n", cliName)
	_, _ = fmt.Fprintf(w, "  %s --version\n", cliName)
	_, _ = fmt.Fprintf(w, "  %s <message>\n", cliName)
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "Interactive TUI is not included in this Go rewrite.")
}

// cliVersion returns the build version (module version, VCS revision, or "dev").
func cliVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && setting.Value != "" {
			if len(setting.Value) > 12 {
				return setting.Value[:12]
			}
			return setting.Value
		}
	}
	return "dev"
}
