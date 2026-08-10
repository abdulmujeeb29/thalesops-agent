package system

import (
	"strings"
	"testing"
)

// Real `ss -tlnp` output from a box running a mix of things. The crucial cases:
// a forking server (gunicorn) whose listener is a worker, several PIDs sharing
// one port, and IPv6 addresses.
const ssTCPSample = `State  Recv-Q Send-Q Local Address:Port  Peer Address:Port Process
LISTEN 0      511          0.0.0.0:80         0.0.0.0:*    users:(("nginx",pid=880,fd=6),("nginx",pid=879,fd=6))
LISTEN 0      128        127.0.0.1:8000       0.0.0.0:*    users:(("gunicorn",pid=1442,fd=5))
LISTEN 0      4096         0.0.0.0:5432       0.0.0.0:*    users:(("docker-proxy",pid=2210,fd=4))
LISTEN 0      511             [::]:3000          [::]:*    users:(("node",pid=3311,fd=19))
`

func TestParseSSExtractsEveryListener(t *testing.T) {
	got, sawPID := parseSS("tcp", ssTCPSample)
	if !sawPID {
		t.Fatal("expected process ownership to be visible in this sample")
	}
	// nginx contributes two PIDs on the same port, so 5 rows from 4 lines.
	if len(got) != 5 {
		t.Fatalf("expected 5 listeners, got %d: %+v", len(got), got)
	}

	var gunicorn *listener
	for i := range got {
		if got[i].Proc == "gunicorn" {
			gunicorn = &got[i]
		}
	}
	if gunicorn == nil {
		t.Fatal("gunicorn listener not parsed")
	}
	if gunicorn.Port != 8000 || gunicorn.Addr != "127.0.0.1" || gunicorn.PID != 1442 {
		t.Errorf("bad gunicorn listener: %+v", gunicorn)
	}
}

func TestParseSSHandlesIPv6Brackets(t *testing.T) {
	got, _ := parseSS("tcp", ssTCPSample)
	for _, l := range got {
		if l.Proc == "node" {
			if l.Port != 3000 {
				t.Errorf("expected port 3000, got %d", l.Port)
			}
			if strings.Contains(l.Addr, "[") {
				t.Errorf("brackets should be stripped from the address, got %q", l.Addr)
			}
			return
		}
	}
	t.Fatal("IPv6 listener not parsed")
}

// Without privileges ss still lists the sockets but omits the users:(...) part.
// We must still return the listeners AND report that ownership was invisible,
// because that's what turns an empty dashboard into an explained one.
func TestParseSSReportsMissingProcessOwnership(t *testing.T) {
	const unprivileged = `State  Recv-Q Send-Q Local Address:Port Peer Address:Port
LISTEN 0      511          0.0.0.0:80        0.0.0.0:*
LISTEN 0      128        127.0.0.1:8000      0.0.0.0:*
`
	got, sawPID := parseSS("tcp", unprivileged)
	if sawPID {
		t.Error("no process info was present, so sawPID must be false")
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 listeners, got %d", len(got))
	}
	if got[0].PID != 0 {
		t.Errorf("expected unknown PID, got %d", got[0].PID)
	}
}

func TestContainerStatusMapping(t *testing.T) {
	cases := []struct {
		name, state, statusStr, want string
	}{
		{"running", "running", "Up 3 hours", statusRunning},
		{"clean exit", "exited", "Exited (0) 2 hours ago", statusStopped},
		{"crash", "exited", "Exited (1) 2 minutes ago", statusFailed},
		{"oom kill", "exited", "Exited (137) 5 seconds ago", statusFailed},
		{"crash loop", "restarting", "Restarting (1) 3 seconds ago", statusFailed},
		{"dead", "dead", "Dead", statusFailed},
		{"created", "created", "Created", statusStopped},
		{"paused", "paused", "Up 2 days (Paused)", statusStopped},
		// Engines too old to expose .State must still be classified.
		{"legacy up", "", "Up 6 minutes", statusRunning},
		{"legacy crash", "", "Exited (2) 1 minute ago", statusFailed},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := containerStatus(c.state, c.statusStr); got != c.want {
				t.Errorf("containerStatus(%q, %q) = %s, want %s", c.state, c.statusStr, got, c.want)
			}
		})
	}
}

func TestParseUnitBlocks(t *testing.T) {
	const raw = `Id=myapp.service
Description=My API
FragmentPath=/etc/systemd/system/myapp.service
ActiveState=active
SubState=running
MainPID=4242

Id=broken.service
Description=Broken worker
FragmentPath=/etc/systemd/system/broken.service
ActiveState=failed
SubState=failed
MainPID=0
`
	units := parseUnitBlocks(raw)
	if len(units) != 2 {
		t.Fatalf("expected 2 units, got %d: %+v", len(units), units)
	}
	if units[0].Name != "myapp.service" || units[0].MainPID != 4242 {
		t.Errorf("first unit parsed wrong: %+v", units[0])
	}
	if got := systemdStatus(units[0]); got != statusRunning {
		t.Errorf("active unit should be RUNNING, got %s", got)
	}
	if got := systemdStatus(units[1]); got != statusFailed {
		t.Errorf("failed unit should be FAILED, got %s", got)
	}
}

func TestOperatorInstalledUnitPaths(t *testing.T) {
	operator := []string{
		"/etc/systemd/system/myapp.service",
		"/usr/local/lib/systemd/system/thing.service",
		"/run/systemd/system/transient.service",
	}
	for _, p := range operator {
		if !isOperatorUnitPath(p) {
			t.Errorf("%s should count as operator-installed", p)
		}
	}
	// Distro units earn a place only by holding a port, never by their path.
	for _, p := range []string{"/lib/systemd/system/ssh.service", "/usr/lib/systemd/system/cron.service", ""} {
		if isOperatorUnitPath(p) {
			t.Errorf("%s should NOT count as operator-installed", p)
		}
	}
}

func TestSystemdSkipList(t *testing.T) {
	for _, unit := range []string{
		"systemd-resolved.service", "user@1000.service", "ssh.service",
		"docker.service", "containerd.service", "thalesops.service", "snap.foo.service",
	} {
		if !systemdSkip(unit) {
			t.Errorf("%s should be skipped", unit)
		}
	}
	// A real app must never be caught by a loose prefix match — "cronicle" is not
	// "cron", and "dockerize-api" is not "docker".
	for _, unit := range []string{
		"myapp.service", "cronicle.service", "dockerize-api.service",
		"gunicorn.service", "sshuttle-vpn.service",
	} {
		if systemdSkip(unit) {
			t.Errorf("%s should NOT be skipped", unit)
		}
	}
}

func TestParseSupervisorStatus(t *testing.T) {
	const raw = `web                              RUNNING   pid 1234, uptime 1:02:03
worker                           FATAL     Exited too quickly (process log may have details)
beat                             STOPPED   Not started
`
	got := parseSupervisorStatus(raw)
	if len(got) != 3 {
		t.Fatalf("expected 3 programs, got %d", len(got))
	}
	if got[0].Identifier != "web" || got[0].Status != statusRunning || got[0].MainPID != 1234 {
		t.Errorf("web parsed wrong: %+v", got[0])
	}
	if got[1].Status != statusFailed {
		t.Errorf("FATAL should map to FAILED, got %s", got[1].Status)
	}
	if got[2].Status != statusStopped {
		t.Errorf("STOPPED should map to STOPPED, got %s", got[2].Status)
	}
}

func TestPM2StatusMapping(t *testing.T) {
	cases := map[string]string{
		"online": statusRunning, "launching": statusRunning,
		"errored": statusFailed, "stopped": statusStopped, "stopping": statusStopped,
	}
	for in, want := range cases {
		if got := pm2Status(in); got != want {
			t.Errorf("pm2Status(%q) = %s, want %s", in, got, want)
		}
	}
}

// The ports string is part of a unique row's rendering and is compared between
// heartbeats, so it must be deterministic regardless of ss's ordering.
func TestFormatListenersIsSortedAndDeduped(t *testing.T) {
	in := []listener{
		{Proto: "tcp", Addr: "::", Port: 8080},
		{Proto: "tcp", Addr: "0.0.0.0", Port: 80},
		{Proto: "tcp", Addr: "0.0.0.0", Port: 80}, // duplicate (two worker PIDs)
	}
	want := "0.0.0.0:80/tcp, [::]:8080/tcp"
	if got := formatListeners(in); got != want {
		t.Errorf("formatListeners = %q, want %q", got, want)
	}

	shuffled := []listener{in[2], in[0], in[1]}
	if got := formatListeners(shuffled); got != want {
		t.Errorf("ordering is not stable: got %q, want %q", got, want)
	}
}

func TestFormatListenersEmpty(t *testing.T) {
	if got := formatListeners(nil); got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

// A scan that under-reports must say so; the dashboard turns notes into a banner
// rather than showing a confidently empty page.
func TestScanNotesDedupeAndFlagDegraded(t *testing.T) {
	s := &scan{claimed: map[int]bool{}}
	s.note("needs root", true)
	s.note("needs root", true)
	s.note("docker missing", false)

	if len(s.notes) != 2 {
		t.Errorf("expected duplicate notes to collapse, got %v", s.notes)
	}
	if !s.degraded {
		t.Error("a degrading note must set degraded")
	}
}

func TestScanAddClaimsPIDs(t *testing.T) {
	s := &scan{claimed: map[int]bool{}}
	s.add(discovered{Source: "SYSTEMD", Identifier: "app.service", MainPID: 99, Status: statusRunning})

	if !s.claimed[99] {
		t.Error("a reported service's PID must be claimed so the process sweep skips it")
	}
	// A blank identifier can't be keyed on and would break the unique constraint.
	s.add(discovered{Source: "PROCESS", Identifier: ""})
	if len(s.items) != 1 {
		t.Errorf("expected the unnamed item to be dropped, got %d items", len(s.items))
	}
}

func TestDetectLooseProcessesSkipsPlumbingAndClaimedPIDs(t *testing.T) {
	s := &scan{claimed: map[int]bool{1442: true}}

	detectLooseProcesses(s, []listener{
		{Proto: "tcp", Addr: "0.0.0.0", Port: 5432, PID: 2210, Proc: "docker-proxy"},
		{Proto: "tcp", Addr: "127.0.0.1", Port: 8000, PID: 1442, Proc: "gunicorn"},
		{Proto: "udp", Addr: "0.0.0.0", Port: 53, PID: 9001, Proc: "dnsmasq"},
	})

	if len(s.items) != 0 {
		t.Errorf("expected no loose processes (docker plumbing, claimed PID, UDP-only), got %+v", s.items)
	}
}

func TestDiscoveredWireOmitsEmptyFields(t *testing.T) {
	w := discovered{Source: "PROCESS", Identifier: "node:3000", Status: statusRunning}.wire()

	for _, key := range []string{"image", "ports", "description", "main_pid", "details"} {
		if _, present := w[key]; present {
			t.Errorf("empty %q should be omitted from the wire payload", key)
		}
	}
	if w["identifier"] != "node:3000" {
		t.Errorf("identifier not carried: %+v", w)
	}
}

func TestWireTruncatesOverlongPorts(t *testing.T) {
	long := strings.Repeat("0.0.0.0:8080/tcp, ", 100)
	w := discovered{Source: "PROCESS", Identifier: "x:1", Status: statusRunning, Ports: long}.wire()

	if len(w["ports"].(string)) > maxPortsLen {
		t.Errorf("ports must be truncated to fit the column, got %d chars", len(w["ports"].(string)))
	}
}
