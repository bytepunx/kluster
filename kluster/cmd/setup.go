package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

var setupDryRun bool

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Check prerequisites and install any that are missing",
	Long: `Checks for required tools (Docker, k3d, kubectl) and installs anything
missing. Docker is never auto-installed — clear instructions are printed instead.

Use --dry-run to preview what would be installed without making changes.`,
	RunE: runSetup,
}

func init() {
	rootCmd.AddCommand(setupCmd)
	setupCmd.Flags().BoolVar(&setupDryRun, "dry-run", false,
		"Print what would be installed without executing")
}

type tool struct {
	name        string
	check       func() (string, bool) // returns (version, found)
	install     func(platform) error
	dockerOnly  bool // true = never auto-install, print instructions instead
}

func runSetup(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	p := detectPlatform()

	if setupDryRun {
		fmt.Fprintln(out, "(dry-run — no changes will be made)")
		fmt.Fprintln(out)
	}

	tools := []tool{
		{
			name:       "docker",
			check:      checkDocker,
			dockerOnly: true,
		},
		{
			// kubectl is optional but expected — users need it to interact with
			// clusters after kluster up. kluster itself uses client-go directly
			// and does not shell out to kubectl.
			name:    "kubectl",
			check:   checkKubectl,
			install: installKubectl,
		},
	}

	allOK := true
	for _, t := range tools {
		version, found := t.check()
		if found {
			fmt.Fprintf(out, "  \033[32m✓\033[0m %s %s\n", t.name, version)
			continue
		}

		allOK = false
		if t.dockerOnly {
			fmt.Fprintf(out, "  \033[31m✗\033[0m %s — not found\n", t.name)
			printDockerInstructions(out, p)
			continue
		}

		fmt.Fprintf(out, "  \033[31m✗\033[0m %s — not found", t.name)
		if setupDryRun {
			fmt.Fprintf(out, " (would install)\n")
			continue
		}
		fmt.Fprintf(out, ", installing...\n")
		if err := t.install(p); err != nil {
			fmt.Fprintf(out, "    \033[31merror:\033[0m %v\n", err)
		} else {
			version, _ := t.check()
			fmt.Fprintf(out, "  \033[32m✓\033[0m %s %s\n", t.name, version)
		}
	}

	if allOK {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "All prerequisites satisfied. Run 'kluster up --profile signet --name dev' to get started.")
	}
	return nil
}

// --- platform detection ---

type platform struct {
	os      string // "darwin", "linux", "windows"
	distro  string // "ubuntu", "debian", "fedora", "arch", "" on non-linux
	isWSL2  bool
	hasBrew bool
}

func detectPlatform() platform {
	p := platform{os: runtime.GOOS}

	if p.os == "darwin" {
		_, err1 := os.Stat("/opt/homebrew/bin/brew")
		_, err2 := os.Stat("/usr/local/bin/brew")
		p.hasBrew = err1 == nil || err2 == nil
		return p
	}

	if p.os == "linux" {
		p.distro = linuxDistro()
		p.isWSL2 = isWSL2()
	}
	return p
}

func linuxDistro() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "ID=") {
			return strings.Trim(strings.TrimPrefix(line, "ID="), `"`)
		}
	}
	return ""
}

func isWSL2() bool {
	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(data)), "microsoft")
}

// --- version checks ---

func checkDocker() (string, bool) {
	if _, err := exec.LookPath("docker"); err != nil {
		return "", false
	}
	out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Output()
	if err != nil {
		return "(daemon not running)", true
	}
	return strings.TrimSpace(string(out)), true
}

func checkKubectl() (string, bool) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		return "", false
	}
	// --short was removed in kubectl v1.28+; parse the first line of plain output.
	out, _ := exec.Command("kubectl", "version", "--client").CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "Client Version:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Client Version:")), true
		}
	}
	return "", true
}

// --- installers ---

func installKubectl(p platform) error {
	switch p.os {
	case "darwin":
		if p.hasBrew {
			return runCmd("brew", "install", "kubectl")
		}
		return fmt.Errorf("homebrew not found — visit https://kubernetes.io/docs/tasks/tools/install-kubectl-macos/")
	case "linux":
		return runCmd("sh", "-c",
			`set -e
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
VERSION=$(curl -sL https://dl.k8s.io/release/stable.txt)
curl -sLo /tmp/kubectl "https://dl.k8s.io/release/${VERSION}/bin/linux/${ARCH}/kubectl"
chmod +x /tmp/kubectl
sudo mv /tmp/kubectl /usr/local/bin/kubectl`)
	default:
		return fmt.Errorf("automatic install not supported on %s — visit https://kubernetes.io/docs/tasks/tools/", p.os)
	}
}

func runCmd(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// --- Docker instructions ---

func printDockerInstructions(out io.Writer, p platform) {
	fmt.Fprintln(out, "    Docker must be installed manually. Instructions:")
	switch p.os {
	case "darwin":
		fmt.Fprintln(out, "    → https://docs.docker.com/desktop/install/mac-install/")
	case "linux":
		if p.isWSL2 {
			fmt.Fprintln(out, "    → Install Docker Desktop for Windows with WSL2 backend:")
			fmt.Fprintln(out, "      https://docs.docker.com/desktop/install/windows-install/")
		} else {
			switch p.distro {
			case "ubuntu", "debian":
				fmt.Fprintln(out, "    → https://docs.docker.com/engine/install/ubuntu/")
			case "fedora":
				fmt.Fprintln(out, "    → https://docs.docker.com/engine/install/fedora/")
			case "arch":
				fmt.Fprintln(out, "    → sudo pacman -S docker && sudo systemctl enable --now docker")
			default:
				fmt.Fprintln(out, "    → https://docs.docker.com/engine/install/")
			}
		}
	default:
		fmt.Fprintln(out, "    → https://docs.docker.com/get-started/get-docker/")
	}
}
