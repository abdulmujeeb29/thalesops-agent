package system

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Discovery finds the deployments already running on a server that ThalesOps did
// NOT deploy, so a user connecting an existing box immediately sees their stuff.
//
// The goal is coverage: "however you deployed it, you should see it". We scan,
// in order, Docker, Podman, systemd, PM2, supervisor, and finally every
// remaining process holding a listening TCP socket. Each layer claims the PIDs
// it owns so the same app is never reported twice.
//
// Everything here is best-effort: a missing binary or a permission error
// degrades one layer and is recorded as a note, never an error that costs us the
// whole scan.

const (
	statusRunning = "RUNNING"
	statusStopped = "STOPPED"
	statusFailed  = "FAILED"

	// Overall wall-clock budget. The heartbeat runs every 60s, so the scan must
	// finish comfortably inside that.
	discoveryBudget = 25 * time.Second

	maxPortsLen   = 500
	maxCmdlineLen = 300
)

// DiscoveryResult is one scan: what we found, plus how well the scan itself went.
type DiscoveryResult struct {
	Services []map[string]interface{}
	Meta     map[string]interface{}
}

type discovered struct {
	Source      string
	Identifier  string
	Image       string
	Ports       string
	Description string
	MainPID     int
	Status      string
	Details     map[string]interface{}
}

func (d discovered) wire() map[string]interface{} {
	m := map[string]interface{}{
		"source":     d.Source,
		"identifier": d.Identifier,
		"status":     d.Status,
	}
	if d.Image != "" {
		m["image"] = d.Image
	}
	if d.Ports != "" {
		m["ports"] = truncate(d.Ports, maxPortsLen)
	}
	if d.Description != "" {
		m["description"] = truncate(d.Description, 255)
	}
	if d.MainPID > 0 {
		m["main_pid"] = d.MainPID
	}
	if len(d.Details) > 0 {
		m["details"] = d.Details
	}
	return m
}

// scan carries the running state of a single discovery pass.
type scan struct {
	items    []discovered
	notes    []string
	degraded bool
	// claimed marks PIDs already attributed to a reported service, so the
	// catch-all process sweep doesn't report the same app a second time.
	claimed map[int]bool
}

// note records something the operator should know about this scan. degraded=true
// means we know we are under-reporting, which the dashboard surfaces as a warning
// rather than letting the page look convincingly empty.
func (s *scan) note(msg string, degraded bool) {
	for _, existing := range s.notes {
		if existing == msg {
			return
		}
	}
	s.notes = append(s.notes, msg)
	if degraded {
		s.degraded = true
	}
}

func (s *scan) add(d discovered) {
	if d.Identifier == "" {
		return
	}
	if d.Status == "" {
		d.Status = statusStopped
	}
	s.items = append(s.items, d)
	if d.MainPID > 0 {
		s.claimed[d.MainPID] = true
	}
}

// Discover runs a full scan of the server.
func Discover() DiscoveryResult {
	ctx, cancel := context.WithTimeout(context.Background(), discoveryBudget)
	defer cancel()

	s := &scan{claimed: map[int]bool{}}

	listeners := collectListeners(ctx, s)

	detectContainers(ctx, s, "docker", "DOCKER")
	detectContainers(ctx, s, "podman", "PODMAN")
	detectSystemd(ctx, s, listeners)
	detectPM2(ctx, s)
	detectSupervisor(ctx, s)
	detectLooseProcesses(s, listeners)

	counts := map[string]int{}
	for _, item := range s.items {
		counts[item.Source]++
	}

	services := make([]map[string]interface{}, 0, len(s.items))
	for _, item := range s.items {
		services = append(services, item.wire())
	}

	return DiscoveryResult{
		Services: services,
		Meta: map[string]interface{}{
			"scanned_at": time.Now().UTC().Format(time.RFC3339),
			"degraded":   s.degraded,
			"notes":      s.notes,
			"counts":     counts,
			"total":      len(services),
		},
	}
}

// ---------------------------------------------------------------------------
// Listening sockets
// ---------------------------------------------------------------------------

type listener struct {
	Proto string
	Addr  string
	Port  int
	PID   int
	Proc  string
}

// ss renders the owning processes as: users:(("nginx",pid=1234,fd=6),...)
var ssProcRe = regexp.MustCompile(`\("([^"]+)",pid=(\d+)`)

// collectListeners returns every listening TCP/UDP socket with its owning PID.
// This is the backbone of discovery: ports are how we find apps that no process
// manager knows about, and how we attach real port numbers to the ones that do.
func collectListeners(ctx context.Context, s *scan) []listener {
	var out []listener
	var ranOK, sawSocket, sawPID bool

	for _, proto := range []string{"tcp", "udp"} {
		flag := "-tlnp"
		if proto == "udp" {
			flag = "-ulnp"
		}
		raw, err := exec.CommandContext(ctx, "ss", flag).Output()
		if err != nil {
			continue
		}
		ranOK = true

		found, withPID := parseSS(proto, string(raw))
		if len(found) > 0 {
			sawSocket = true
		}
		if withPID {
			sawPID = true
		}
		out = append(out, found...)
	}

	switch {
	case !ranOK:
		s.note("`ss` is not installed on this server, so port-based discovery is off. "+
			"Install iproute2 to detect apps that aren't in Docker or a process manager.", true)
	case sawSocket && !sawPID:
		s.note("The agent can't map listening ports to the processes that own them. "+
			"Run the agent as root to discover systemd services and standalone processes.", true)
	}

	return out
}

// parseSS turns one `ss -tlnp` / `ss -ulnp` dump into listeners. The second
// return reports whether ANY socket named its owning process — when nothing does,
// we're running unprivileged and are blind to who owns which port.
func parseSS(proto, raw string) ([]listener, bool) {
	var out []listener
	sawPID := false

	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		// Skip the header row, whichever column layout this ss prints.
		if strings.EqualFold(fields[0], "State") || strings.EqualFold(fields[0], "Netid") {
			continue
		}

		local := fields[3]
		cut := strings.LastIndex(local, ":")
		if cut < 0 {
			continue
		}
		port, err := strconv.Atoi(local[cut+1:])
		if err != nil || port <= 0 {
			continue
		}
		addr := strings.Trim(local[:cut], "[]")

		matches := ssProcRe.FindAllStringSubmatch(line, -1)
		if len(matches) == 0 {
			// Listening, but we can't see who owns it.
			out = append(out, listener{Proto: proto, Addr: addr, Port: port})
			continue
		}
		for _, m := range matches {
			pid, _ := strconv.Atoi(m[2])
			sawPID = true
			out = append(out, listener{Proto: proto, Addr: addr, Port: port, PID: pid, Proc: m[1]})
		}
	}
	return out, sawPID
}

// ---------------------------------------------------------------------------
// Containers (Docker / Podman)
// ---------------------------------------------------------------------------

var exitCodeRe = regexp.MustCompile(`Exited \((\d+)\)`)

// detectContainers reports every container on the host, including stopped and
// crashed ones — `docker ps` alone hides exactly the containers a user most
// needs to see. ThalesOps-managed containers are excluded via our own labels.
func detectContainers(ctx context.Context, s *scan, bin, source string) {
	if _, err := exec.LookPath(bin); err != nil {
		return
	}

	const format = `{{.Names}}|{{.Image}}|{{.Ports}}|{{.Status}}|{{.State}}|` +
		`{{.Label "thalesops.app"}}|{{.Label "thalesops.db"}}|` +
		`{{.Label "com.docker.compose.project"}}|{{.Label "com.docker.compose.service"}}`

	raw, err := exec.CommandContext(ctx, bin, "ps", "-a", "--format", format).Output()
	if err != nil {
		s.note(fmt.Sprintf("`%s ps` failed, so %s deployments were not scanned "+
			"(is the daemon running and the agent in the right group?).", bin, source), true)
		return
	}

	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) < 9 {
			continue
		}
		name, image, ports := parts[0], parts[1], parts[2]
		statusStr, state := parts[3], parts[4]
		appLabel, dbLabel := parts[5], parts[6]
		project, service := parts[7], parts[8]

		if name == "" {
			continue
		}
		if strings.TrimSpace(appLabel) != "" || strings.TrimSpace(dbLabel) != "" {
			continue // ours
		}

		details := map[string]interface{}{"status_string": statusStr, "state": state}
		if strings.TrimSpace(project) != "" {
			details["compose_project"] = project
			details["compose_service"] = service
		}

		s.add(discovered{
			Source:     source,
			Identifier: name,
			Image:      image,
			Ports:      ports,
			Status:     containerStatus(state, statusStr),
			Details:    details,
		})
	}
}

// containerStatus maps an engine's state to ours, treating a non-zero exit and a
// restart loop as FAILED so a broken app doesn't read as a clean "Stopped".
func containerStatus(state, statusStr string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "running":
		return statusRunning
	case "restarting", "dead":
		return statusFailed
	case "created", "paused", "exited":
		if m := exitCodeRe.FindStringSubmatch(statusStr); m != nil && m[1] != "0" {
			return statusFailed
		}
		return statusStopped
	}

	// Older engines don't expose .State — read the human status instead.
	low := strings.ToLower(strings.TrimSpace(statusStr))
	switch {
	case strings.HasPrefix(low, "up"):
		return statusRunning
	case strings.HasPrefix(low, "restarting"), strings.HasPrefix(low, "dead"):
		return statusFailed
	case strings.HasPrefix(low, "exited"):
		if m := exitCodeRe.FindStringSubmatch(statusStr); m != nil && m[1] != "0" {
			return statusFailed
		}
	}
	return statusStopped
}

// ---------------------------------------------------------------------------
// systemd
// ---------------------------------------------------------------------------

// Unit families that are never a user's deployment. Prefix matches.
//
// Deliberately NOT "thalesops" — that prefix would also hide a user's own app
// running as thalesops.service. Only this agent's own unit is skipped, by exact
// name, in systemdSkipExact below.
var systemdSkipPrefixes = []string{
	"systemd-", "user@", "user-runtime-dir@", "session-", "getty@", "serial-getty@",
	"snap.",
}

// Distro daemons that listen on ports but aren't deployments. Exact matches, so
// a real app named e.g. "cronicle.service" is never caught by a "cron" prefix.
var systemdSkipExact = map[string]bool{
	"ssh.service": true, "sshd.service": true, "cron.service": true, "crond.service": true,
	"atd.service": true, "dbus.service": true, "dbus-broker.service": true,
	"polkit.service": true, "rsyslog.service": true, "acpid.service": true,
	"irqbalance.service": true, "thermald.service": true, "udisks2.service": true,
	"accounts-daemon.service": true, "ModemManager.service": true,
	"NetworkManager.service": true, "networkd-dispatcher.service": true,
	"unattended-upgrades.service": true, "apparmor.service": true, "auditd.service": true,
	"multipathd.service": true, "packagekit.service": true, "snapd.service": true,
	"rpcbind.service": true, "chrony.service": true, "chronyd.service": true,
	"ntp.service": true, "ntpd.service": true, "emergency.service": true,
	"rescue.service": true, "console-setup.service": true, "keyboard-setup.service": true,
	"setvtrgb.service": true, "e2scrub_reap.service": true, "blk-availability.service": true,
	"fail2ban.service": true, "cloud-init.service": true, "cloud-config.service": true,
	"cloud-final.service": true, "qemu-guest-agent.service": true,
	"amazon-ssm-agent.service": true, "google-guest-agent.service": true,
	"hv-kvp-daemon.service": true, "open-vm-tools.service": true, "walinuxagent.service": true,
	// Container runtimes: their supervisor processes (docker-proxy et al) hold the
	// published ports, which would otherwise make the runtime itself look like an
	// app listening on every container's port.
	"docker.service": true, "containerd.service": true, "podman.service": true,
	"cri-docker.service": true,
	// This agent itself (see install.sh SERVICE_NAME). Matched exactly, never by
	// prefix — a user's own app may legitimately be called thalesops.service.
	"thalesops-agent.service": true,
}

func systemdSkip(unit string) bool {
	if systemdSkipExact[unit] {
		return true
	}
	for _, p := range systemdSkipPrefixes {
		if strings.HasPrefix(unit, p) {
			return true
		}
	}
	return false
}

type unitInfo struct {
	Name         string
	Description  string
	FragmentPath string
	ActiveState  string
	SubState     string
	MainPID      int
}

// detectSystemd reports units that are either operator-installed (their unit file
// lives in /etc/systemd/system and friends) or currently holding a listening
// port. The second rule is what catches an app installed from a distro package,
// and the port attribution is done through each process's cgroup — so a forking
// server like gunicorn or php-fpm, where the listener is a *child* of MainPID,
// is still matched to its unit.
func detectSystemd(ctx context.Context, s *scan, listeners []listener) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return
	}

	raw, err := exec.CommandContext(ctx, "systemctl",
		"list-units", "--type=service", "--all", "--no-legend", "--plain", "--no-pager").Output()
	if err != nil {
		s.note("`systemctl list-units` failed, so systemd deployments were not scanned.", true)
		return
	}

	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		fields := strings.Fields(strings.TrimLeft(line, "●* \t"))
		if len(fields) == 0 {
			continue
		}
		unit := fields[0]
		if !strings.HasSuffix(unit, ".service") || systemdSkip(unit) {
			continue
		}
		names = append(names, unit)
	}
	if len(names) == 0 {
		return
	}

	units := showUnits(ctx, names)

	// Attribute every listening port to the unit that actually owns it.
	portsByUnit := map[string][]listener{}
	for _, l := range listeners {
		if l.PID == 0 {
			continue
		}
		kind, unit := procOwner(l.PID)
		if kind != "systemd" || unit == "" {
			continue
		}
		portsByUnit[unit] = append(portsByUnit[unit], l)
		// Claim it either way: a PID belonging to a unit is systemd's business,
		// even when we filter that unit out as a system daemon.
		s.claimed[l.PID] = true
	}

	for _, u := range units {
		operatorInstalled := isOperatorUnitPath(u.FragmentPath)
		unitListeners := portsByUnit[u.Name]

		// Neither operator-installed nor serving traffic → not a deployment.
		if !operatorInstalled && len(unitListeners) == 0 {
			continue
		}
		// A distro unit only earns a place while it's actually serving.
		if !operatorInstalled && u.ActiveState != "active" {
			continue
		}

		s.add(discovered{
			Source:      "SYSTEMD",
			Identifier:  u.Name,
			Description: u.Description,
			Ports:       formatListeners(unitListeners),
			MainPID:     u.MainPID,
			Status:      systemdStatus(u),
			Details: map[string]interface{}{
				"fragment_path":      u.FragmentPath,
				"active_state":       u.ActiveState,
				"sub_state":          u.SubState,
				"operator_installed": operatorInstalled,
			},
		})
	}
}

func isOperatorUnitPath(path string) bool {
	for _, prefix := range []string{
		"/etc/systemd/system/",
		"/usr/local/lib/systemd/system/",
		"/run/systemd/system/",
	} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func systemdStatus(u unitInfo) string {
	switch {
	case u.ActiveState == "failed" || u.SubState == "failed":
		return statusFailed
	case u.ActiveState == "active" || u.ActiveState == "activating" || u.ActiveState == "reloading":
		return statusRunning
	default:
		return statusStopped
	}
}

// showUnits reads every unit's properties in ONE systemctl call — per-unit calls
// cost a subprocess each and a busy box has hundreds of units.
func showUnits(ctx context.Context, names []string) []unitInfo {
	args := append([]string{"show", "--no-pager",
		"-p", "Id", "-p", "Description", "-p", "FragmentPath",
		"-p", "ActiveState", "-p", "SubState", "-p", "MainPID"}, names...)

	raw, err := exec.CommandContext(ctx, "systemctl", args...).Output()
	if err != nil {
		return nil
	}
	return parseUnitBlocks(string(raw))
}

// parseUnitBlocks reads `systemctl show`'s Key=Value output, where a blank line
// separates one unit's properties from the next.
func parseUnitBlocks(raw string) []unitInfo {
	var units []unitInfo
	current := unitInfo{}
	flush := func() {
		if current.Name != "" {
			units = append(units, current)
		}
		current = unitInfo{}
	}

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			flush() // blank line separates unit blocks
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch key {
		case "Id":
			current.Name = value
		case "Description":
			current.Description = value
		case "FragmentPath":
			current.FragmentPath = value
		case "ActiveState":
			current.ActiveState = value
		case "SubState":
			current.SubState = value
		case "MainPID":
			current.MainPID, _ = strconv.Atoi(value)
		}
	}
	flush()
	return units
}

// procOwner attributes a PID to whatever supervises it, by reading its cgroup.
// Returns ("container", "") for anything under a container runtime, or
// ("systemd", "<unit>.service") for a systemd-managed process.
var cgUnitRe = regexp.MustCompile(`([a-zA-Z0-9@:_\-\\.]+\.service)`)

func procOwner(pid int) (string, string) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return "", ""
	}
	content := string(raw)
	for _, marker := range []string{"docker", "libpod", "containerd", "kubepods", "crio-"} {
		if strings.Contains(content, marker) {
			return "container", ""
		}
	}
	if m := cgUnitRe.FindStringSubmatch(content); m != nil {
		unit := m[1]
		// user@1000.service is a login session, not a deployment — the processes
		// under it are ordinary user processes and belong to the catch-all sweep.
		if !strings.HasPrefix(unit, "user@") {
			return "systemd", unit
		}
	}
	return "", ""
}

// ---------------------------------------------------------------------------
// PM2
// ---------------------------------------------------------------------------

type pm2Process struct {
	Name string `json:"name"`
	PID  int    `json:"pid"`
	Env  struct {
		Status      string `json:"status"`
		RestartTime int    `json:"restart_time"`
		ExecPath    string `json:"pm_exec_path"`
		Cwd         string `json:"pm_cwd"`
		ExecMode    string `json:"exec_mode"`
	} `json:"pm2_env"`
}

// detectPM2 reads each PM2 daemon's process list. PM2 is per-user, so when we're
// root we sweep every user running a PM2 daemon — otherwise a Node app deployed
// under a `deploy` user would be invisible.
func detectPM2(ctx context.Context, s *scan) {
	if _, err := exec.LookPath("pm2"); err != nil {
		return
	}

	seen := map[string]bool{}
	report := func(owner string, procs []pm2Process) {
		for _, p := range procs {
			if p.Name == "" {
				continue
			}
			// Namespace other users' apps so two users' "api" don't collide.
			identifier := p.Name
			if owner != "" {
				identifier = owner + "/" + p.Name
			}
			if seen[identifier] {
				continue
			}
			seen[identifier] = true

			s.add(discovered{
				Source:      "PM2",
				Identifier:  identifier,
				Description: p.Env.ExecPath,
				MainPID:     p.PID,
				Status:      pm2Status(p.Env.Status),
				Details: map[string]interface{}{
					"pm2_status":   p.Env.Status,
					"restarts":     p.Env.RestartTime,
					"exec_path":    p.Env.ExecPath,
					"cwd":          p.Env.Cwd,
					"exec_mode":    p.Env.ExecMode,
					"pm2_owner":    owner,
					"process_name": p.Name,
				},
			})
		}
	}

	// The agent's own user first.
	if procs, ok := pm2List(ctx, ""); ok {
		report("", procs)
	}

	if os.Geteuid() != 0 {
		return
	}
	for _, owner := range pm2DaemonUsers(ctx) {
		if procs, ok := pm2List(ctx, owner); ok {
			report(owner, procs)
		}
	}
}

func pm2Status(status string) string {
	switch strings.ToLower(status) {
	case "online", "launching":
		return statusRunning
	case "errored":
		return statusFailed
	default:
		return statusStopped
	}
}

// pm2List runs `pm2 jlist` (as another user when owner is set) and parses it.
func pm2List(ctx context.Context, owner string) ([]pm2Process, bool) {
	callCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	if owner == "" {
		cmd = exec.CommandContext(callCtx, "pm2", "jlist")
	} else {
		cmd = exec.CommandContext(callCtx, "su", "-", owner, "-c", "pm2 jlist")
	}

	raw, err := cmd.Output()
	if err != nil {
		return nil, false
	}
	// PM2 sometimes prints a banner before the JSON.
	start := strings.Index(string(raw), "[")
	if start < 0 {
		return nil, false
	}

	var procs []pm2Process
	if err := json.Unmarshal(raw[start:], &procs); err != nil {
		return nil, false
	}
	return procs, true
}

// pm2DaemonUsers finds the users with a running PM2 daemon.
func pm2DaemonUsers(ctx context.Context) []string {
	raw, err := exec.CommandContext(ctx, "ps", "-eo", "user:32=,args=").Output()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var users []string
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, "PM2") && !strings.Contains(line, "God Daemon") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		owner := fields[0]
		if owner == "root" || seen[owner] {
			continue // root's list was already read directly
		}
		seen[owner] = true
		users = append(users, owner)
	}
	return users
}

// ---------------------------------------------------------------------------
// supervisor
// ---------------------------------------------------------------------------

// detectSupervisor parses `supervisorctl status`, e.g.
//
//	web    RUNNING   pid 1234, uptime 1:02:03
//	worker FATAL     Exited too quickly
func detectSupervisor(ctx context.Context, s *scan) {
	if _, err := exec.LookPath("supervisorctl"); err != nil {
		return
	}

	callCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	// supervisorctl exits non-zero when any program isn't RUNNING, so the output
	// matters more than the exit code here.
	raw, err := exec.CommandContext(callCtx, "supervisorctl", "status").Output()
	if len(raw) == 0 && err != nil {
		return
	}

	for _, d := range parseSupervisorStatus(string(raw)) {
		s.add(d)
	}
}

func parseSupervisorStatus(raw string) []discovered {
	var out []discovered

	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name, state := fields[0], strings.ToUpper(fields[1])

		pid := 0
		for i := 0; i < len(fields)-1; i++ {
			if fields[i] == "pid" {
				pid, _ = strconv.Atoi(strings.TrimSuffix(fields[i+1], ","))
				break
			}
		}

		out = append(out, discovered{
			Source:      "SUPERVISOR",
			Identifier:  name,
			Description: strings.Join(fields[2:], " "),
			MainPID:     pid,
			Status:      supervisorStatus(state),
			Details:     map[string]interface{}{"supervisor_state": state},
		})
	}
	return out
}

func supervisorStatus(state string) string {
	switch state {
	case "RUNNING", "STARTING":
		return statusRunning
	case "FATAL", "BACKOFF", "UNKNOWN":
		return statusFailed
	default: // STOPPED, EXITED, STOPPING
		return statusStopped
	}
}

// ---------------------------------------------------------------------------
// Catch-all: anything else holding a port
// ---------------------------------------------------------------------------

// Processes that hold ports on behalf of something we already reported, or that
// are plumbing rather than a deployment.
var looseProcSkip = map[string]bool{
	"docker-proxy": true, "dockerd": true, "containerd": true, "conmon": true,
	"podman": true, "rootlesskit": true, "slirp4netns": true, "containerd-shim": true,
	"systemd-resolve": true, "systemd-resolved": true, "systemd-network": true,
	"sshd": true, "init": true, "systemd": true,
}

// detectLooseProcesses is the safety net that makes discovery honest: any process
// still holding a listening TCP socket after every process manager has had its
// turn. This is what catches `nohup`, `screen`, `tmux`, a hand-rolled rc.local,
// or a binary someone started over SSH two months ago.
func detectLooseProcesses(s *scan, listeners []listener) {
	byPID := map[int][]listener{}
	for _, l := range listeners {
		if l.Proto != "tcp" || l.PID == 0 || s.claimed[l.PID] {
			continue
		}
		if looseProcSkip[l.Proc] {
			continue
		}
		// Anything under a container runtime or a systemd unit was already
		// handled (or deliberately filtered) by the layers above.
		if kind, _ := procOwner(l.PID); kind != "" {
			continue
		}
		byPID[l.PID] = append(byPID[l.PID], l)
	}

	pids := make([]int, 0, len(byPID))
	for pid := range byPID {
		pids = append(pids, pid)
	}
	sort.Ints(pids)

	for _, pid := range pids {
		group := byPID[pid]
		name := group[0].Proc
		if name == "" {
			name = "process"
		}

		// Identify by name+lowest port, not PID: a restart changes the PID but
		// should stay the same row in the dashboard.
		lowest := group[0].Port
		for _, l := range group {
			if l.Port < lowest {
				lowest = l.Port
			}
		}

		details := map[string]interface{}{"pid": pid, "process_name": name}
		if cmdline := procCmdline(pid); cmdline != "" {
			details["cmdline"] = cmdline
		}
		if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
			details["exe"] = exe
		}
		if cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid)); err == nil {
			details["cwd"] = cwd
		}
		if owner := procUser(pid); owner != "" {
			details["user"] = owner
		}

		s.add(discovered{
			Source:      "PROCESS",
			Identifier:  fmt.Sprintf("%s:%d", name, lowest),
			Description: procCmdline(pid),
			Ports:       formatListeners(group),
			MainPID:     pid,
			Status:      statusRunning, // it holds an open socket right now
			Details:     details,
		})
	}
}

func procCmdline(pid int) string {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	cmd := strings.TrimSpace(strings.ReplaceAll(string(raw), "\x00", " "))
	return truncate(cmd, maxCmdlineLen)
}

func procUser(pid int) string {
	info, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	if err != nil {
		return ""
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	uid := strconv.FormatUint(uint64(stat.Uid), 10)
	if u, err := user.LookupId(uid); err == nil {
		return u.Username
	}
	return uid
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// formatListeners renders a set of sockets as "0.0.0.0:8080/tcp, [::]:8080/tcp",
// deduped and ordered by port so the string is stable between heartbeats.
func formatListeners(ls []listener) string {
	seen := map[string]bool{}
	type entry struct {
		port int
		text string
	}
	var entries []entry

	for _, l := range ls {
		addr := l.Addr
		if strings.Contains(addr, ":") {
			addr = "[" + addr + "]"
		}
		text := fmt.Sprintf("%s:%d/%s", addr, l.Port, l.Proto)
		if seen[text] {
			continue
		}
		seen[text] = true
		entries = append(entries, entry{port: l.Port, text: text})
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].port != entries[j].port {
			return entries[i].port < entries[j].port
		}
		return entries[i].text < entries[j].text
	})

	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, e.text)
	}
	return strings.Join(parts, ", ")
}

// truncate caps a string at max BYTES, ellipsis included — these values go into
// fixed-width database columns, and "…" is three bytes, not one. The cut is
// pulled back to a rune boundary so we never emit invalid UTF-8.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const ellipsis = "…"
	cut := max - len(ellipsis)
	if cut <= 0 {
		return s[:max]
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ellipsis
}
