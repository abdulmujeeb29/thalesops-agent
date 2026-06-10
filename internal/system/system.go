// Package system inspects the host for the tools the agent needs to deploy
// (Docker, Nixpacks) and reports real capabilities to the backend.
package system

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// lookPath reports whether a binary is available on PATH.
func lookPath(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

// version runs `<bin> <verArg>` and returns the trimmed first line, or "".
func version(bin, verArg string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, verArg).Output()
	if err != nil {
		return ""
	}
	line := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	return line
}

func HasDocker() bool   { return lookPath("docker") }
func HasNixpacks() bool { return lookPath("nixpacks") }

// Capabilities returns the real, detected capabilities of this host.
// Replaces the old hardcoded {shell, docker} so the dashboard knows the truth.
func Capabilities() map[string]interface{} {
	caps := map[string]interface{}{
		"shell":  true,
		"docker": HasDocker(),
		"nixpacks": HasNixpacks(),
	}
	if v := version("docker", "--version"); v != "" {
		caps["docker_version"] = v
	}
	if v := version("nixpacks", "--version"); v != "" {
		caps["nixpacks_version"] = v
	}
	caps["deploy_ready"] = HasDocker() && HasNixpacks()
	return caps
}

// EnsureDeployPrerequisites verifies the tools needed for a deployment are
// present. Returns a clear, user-facing error if something is missing so the
// failure shows up meaningfully in the deployment logs (rather than a cryptic
// "command not found"). Installation itself is handled by install.sh at agent
// install time; this is the defensive guard before each deploy.
func EnsureDeployPrerequisites() error {
	var missing []string
	if !HasDocker() {
		missing = append(missing, "docker")
	}
	if !HasNixpacks() {
		missing = append(missing, "nixpacks")
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"missing required tools on this server: %s. "+
				"Re-run the ThalesOps install script to set them up",
			strings.Join(missing, ", "),
		)
	}

	// Docker present but daemon not running is a common failure — surface it clearly.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		return fmt.Errorf("docker is installed but the daemon is not reachable: %w", err)
	}
	return nil
}
