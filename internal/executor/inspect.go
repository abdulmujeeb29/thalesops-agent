package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/thalesops/agent/internal/models"
)

// Inspection reads everything needed to ADOPT an external deployment — to
// recreate it as a managed application and take over its deploys.
//
// The prize is the app's working directory: if it's a git checkout we can read
// its origin remote and match it to one of the user's repos, which tells us
// exactly what the app is. Env vars come along so the managed version boots with
// the same configuration.
//
// This runs ONLY when a user explicitly asks for it, never on the heartbeat —
// it reads secrets out of a running process, and that should be a deliberate act
// with the results shown before anything is stored.

const inspectTimeout = 30 * time.Second

// ExecuteInspectDiscovery gathers adoption data for one discovered service.
// The JSON result rides back as the command's stdout.
func ExecuteInspectDiscovery(payload map[string]interface{}) models.CommandResultRequest {
	ctx, cancel := context.WithTimeout(context.Background(), inspectTimeout)
	defer cancel()

	source, _ := payload["source"].(string)
	identifier, _ := payload["identifier"].(string)
	includeEnv, _ := payload["include_env"].(bool)

	pid := 0
	if f, ok := payload["main_pid"].(float64); ok {
		pid = int(f)
	}

	result := models.DiscoveryInspection{
		Source:     source,
		Identifier: identifier,
		Env:        map[string]string{},
	}

	switch source {
	case "DOCKER", "PODMAN":
		bin := "docker"
		if source == "PODMAN" {
			bin = "podman"
		}
		inspectContainer(ctx, bin, identifier, &result)
	case "SYSTEMD":
		inspectSystemdUnit(ctx, identifier, &result)
	case "PM2", "SUPERVISOR", "PROCESS":
		inspectProcess(pid, &result)
	default:
		return failedInspection(fmt.Sprintf("don't know how to inspect a %s deployment", source))
	}

	// The working directory is what turns "something is running" into "this is
	// your repo at this commit".
	if result.WorkDir != "" {
		result.Git = readGitCheckout(ctx, result.WorkDir)
		if result.Git == nil {
			result.Notes = append(result.Notes, fmt.Sprintf(
				"%s isn't a git checkout, so we couldn't identify the source repository.",
				result.WorkDir))
		}
	} else if result.Source != "DOCKER" && result.Source != "PODMAN" {
		result.Notes = append(result.Notes,
			"Couldn't determine this deployment's working directory, so its source repository is unknown.")
	}

	// Secrets only travel when the user asked for them.
	if !includeEnv {
		result.Env = map[string]string{}
		result.EnvOmitted = true
	}

	body, err := json.Marshal(result)
	if err != nil {
		return failedInspection(fmt.Sprintf("could not encode inspection: %v", err))
	}
	return models.CommandResultRequest{ExitCode: 0, Stdout: string(body)}
}

func failedInspection(msg string) models.CommandResultRequest {
	return models.CommandResultRequest{ExitCode: 1, Stderr: msg}
}

// ---------------------------------------------------------------------------
// Containers
// ---------------------------------------------------------------------------

// containerInspect is the slice of `docker inspect` we care about.
type containerInspect struct {
	Config struct {
		Env        []string          `json:"Env"`
		Labels     map[string]string `json:"Labels"`
		Image      string            `json:"Image"`
		WorkingDir string            `json:"WorkingDir"`
		Cmd        []string          `json:"Cmd"`
	} `json:"Config"`
	Mounts []struct {
		Type        string `json:"Type"`
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
		Name        string `json:"Name"`
	} `json:"Mounts"`
	HostConfig struct {
		RestartPolicy struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
}

func inspectContainer(ctx context.Context, bin, name string, out *models.DiscoveryInspection) {
	raw, err := exec.CommandContext(ctx, bin, "inspect", name).Output()
	if err != nil {
		out.Notes = append(out.Notes, fmt.Sprintf("`%s inspect %s` failed.", bin, name))
		return
	}

	var items []containerInspect
	if err := json.Unmarshal(raw, &items); err != nil || len(items) == 0 {
		out.Notes = append(out.Notes, "Could not parse the container's configuration.")
		return
	}
	c := items[0]

	out.Image = c.Config.Image
	out.RestartPolicy = c.HostConfig.RestartPolicy.Name
	out.Command = strings.Join(c.Config.Cmd, " ")

	for _, entry := range c.Config.Env {
		if key, value, ok := strings.Cut(entry, "="); ok {
			out.Env[key] = value
		}
	}

	// A container built elsewhere usually has no source on this host. Two labels
	// can still point us at the repo.
	if src := c.Config.Labels["org.opencontainers.image.source"]; src != "" {
		out.SourceLabel = src
	}
	// Compose records the directory the stack was launched from — on a server
	// that deployed from a clone, that directory IS the repo.
	if dir := c.Config.Labels["com.docker.compose.project.working_dir"]; dir != "" {
		out.WorkDir = dir
	}

	// Mounted data does NOT come across to a fresh managed deploy. Name them so
	// the user is warned before they cut over.
	for _, m := range c.Mounts {
		ref := m.Source
		if m.Type == "volume" && m.Name != "" {
			ref = m.Name
		}
		out.Volumes = append(out.Volumes, fmt.Sprintf("%s → %s", ref, m.Destination))
	}
}

// ---------------------------------------------------------------------------
// systemd
// ---------------------------------------------------------------------------

func inspectSystemdUnit(ctx context.Context, unit string, out *models.DiscoveryInspection) {
	raw, err := exec.CommandContext(ctx, "systemctl", "show", "--no-pager",
		"-p", "WorkingDirectory", "-p", "ExecStart", "-p", "Environment",
		"-p", "EnvironmentFiles", "-p", "User", "-p", "FragmentPath", unit).Output()
	if err != nil {
		out.Notes = append(out.Notes, fmt.Sprintf("`systemctl show %s` failed.", unit))
		return
	}

	var envFiles []string
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(strings.TrimRight(line, "\r"), "=")
		if !ok {
			continue
		}
		switch key {
		case "WorkingDirectory":
			out.WorkDir = value
		case "ExecStart":
			out.Command = value
		case "User":
			out.RunAsUser = value
		case "FragmentPath":
			out.UnitFile = value
		case "Environment":
			for k, v := range parseInlineEnv(value) {
				out.Env[k] = v
			}
		case "EnvironmentFiles":
			envFiles = append(envFiles, parseEnvironmentFiles(value)...)
		}
	}

	for _, path := range envFiles {
		for k, v := range readEnvFile(path) {
			out.Env[k] = v
		}
	}

	// A unit is often "ExecStart=/usr/bin/node /srv/app/index.js" with no
	// WorkingDirectory set — the binary's directory is the next best guess.
	if out.WorkDir == "" {
		out.WorkDir = guessWorkDirFromCommand(out.Command)
	}
}

// systemd renders Environment as: Environment=FOO=1 BAR="two words"
var inlineEnvRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)=("([^"]*)"|'([^']*)'|[^\s]*)`)

func parseInlineEnv(value string) map[string]string {
	env := map[string]string{}
	for _, m := range inlineEnvRe.FindAllStringSubmatch(value, -1) {
		val := m[2]
		if m[3] != "" || strings.HasPrefix(m[2], `"`) {
			val = m[3]
		} else if m[4] != "" || strings.HasPrefix(m[2], `'`) {
			val = m[4]
		}
		env[m[1]] = val
	}
	return env
}

// EnvironmentFiles renders as: EnvironmentFiles=/etc/app.env (ignore_errors=no)
func parseEnvironmentFiles(value string) []string {
	var paths []string
	for _, field := range strings.Fields(value) {
		if strings.HasPrefix(field, "/") {
			paths = append(paths, field)
		}
	}
	return paths
}

func readEnvFile(path string) map[string]string {
	env := map[string]string{}
	raw, err := os.ReadFile(path)
	if err != nil {
		return env
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		if key, value, ok := strings.Cut(line, "="); ok {
			env[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return env
}

// guessWorkDirFromCommand pulls the first absolute path out of a command line and
// returns its directory.
func guessWorkDirFromCommand(command string) string {
	for _, field := range strings.Fields(command) {
		field = strings.Trim(field, `"'`)
		if !strings.HasPrefix(field, "/") {
			continue
		}
		if info, err := os.Stat(field); err == nil {
			if info.IsDir() {
				return field
			}
			if idx := strings.LastIndex(field, "/"); idx > 0 {
				return field[:idx]
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Plain processes (PM2 / supervisor / standalone)
// ---------------------------------------------------------------------------

func inspectProcess(pid int, out *models.DiscoveryInspection) {
	if pid <= 0 {
		out.Notes = append(out.Notes,
			"This deployment has no running process to inspect — start it and try again.")
		return
	}

	if cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid)); err == nil {
		out.WorkDir = cwd
	}
	if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		out.Binary = exe
	}
	if raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		out.Command = strings.TrimSpace(strings.ReplaceAll(string(raw), "\x00", " "))
	}

	// /proc/<pid>/environ is the process's env AS IT WAS STARTED. Readable only
	// by root or the process owner.
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil {
		out.Notes = append(out.Notes,
			"Couldn't read this process's environment (needs root) — you'll have to enter its variables by hand.")
		return
	}
	for _, entry := range strings.Split(string(raw), "\x00") {
		if key, value, ok := strings.Cut(entry, "="); ok && key != "" {
			out.Env[key] = value
		}
	}
}

// ---------------------------------------------------------------------------
// git
// ---------------------------------------------------------------------------

// readGitCheckout identifies the repo, branch and exact commit a directory is on.
// The commit matters as much as the repo: adopting should redeploy the code that
// is ALREADY running, not silently jump the user to the branch head.
func readGitCheckout(ctx context.Context, dir string) *models.GitCheckout {
	run := func(args ...string) string {
		full := append([]string{"-C", dir}, args...)
		out, err := exec.CommandContext(ctx, "git", full...).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}

	if run("rev-parse", "--is-inside-work-tree") != "true" {
		return nil
	}

	remote := run("remote", "get-url", "origin")
	if remote == "" {
		return nil
	}

	checkout := &models.GitCheckout{
		Remote: sanitizeRemote(remote),
		Branch: run("rev-parse", "--abbrev-ref", "HEAD"),
		Commit: run("rev-parse", "HEAD"),
	}
	// Uncommitted work will NOT be in the managed build — the user has to push
	// first, so we surface it rather than silently losing it.
	checkout.Dirty = run("status", "--porcelain") != ""

	if checkout.Branch == "HEAD" {
		checkout.Branch = "" // detached; the caller falls back to the repo default
	}
	return checkout
}

// credentialsRe strips any embedded token from a remote URL — clone URLs on
// servers frequently carry one, and it must never be shipped or stored.
var credentialsRe = regexp.MustCompile(`//[^/@]*@`)

func sanitizeRemote(remote string) string {
	return credentialsRe.ReplaceAllString(remote, "//")
}
