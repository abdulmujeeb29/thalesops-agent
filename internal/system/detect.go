package system

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// DetectedDatabase is a database already present on the server that ThalesOps
// did NOT provision — either a host process on a well-known port, or a Docker
// container that isn't one of ours. Surfaced to the dashboard so we can offer
// "connect your existing database" instead of only "provision a new one".
type DetectedDatabase struct {
	Engine string `json:"engine"` // postgres / mysql / redis
	Source string `json:"source"` // "host:5432" or "container:my-pg"
}

// wellKnownDBPorts maps a listening host port to its engine.
var wellKnownDBPorts = map[string]string{
	"5432": "postgres",
	"3306": "mysql",
	"6379": "redis",
}

// dbImageEngines maps a substring of a Docker image name to its engine.
var dbImageEngines = map[string]string{
	"postgres": "postgres",
	"mysql":    "mysql",
	"mariadb":  "mysql",
	"redis":    "redis",
}

// DetectDatabases inspects the host for databases we don't manage: processes
// listening on well-known DB ports, and non-ThalesOps database containers.
// Cheap (two short commands) and best-effort — any error yields no findings.
func DetectDatabases() []DetectedDatabase {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var found []DetectedDatabase
	seen := map[string]bool{}
	add := func(engine, source string) {
		key := engine + "|" + source
		if !seen[key] {
			seen[key] = true
			found = append(found, DetectedDatabase{Engine: engine, Source: source})
		}
	}

	// 1. Host processes listening on well-known DB ports.
	if out, err := exec.CommandContext(ctx, "ss", "-ltn").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			for port, engine := range wellKnownDBPorts {
				// Match ":<port> " so 5432 doesn't match 15432, etc.
				if strings.Contains(line, ":"+port+" ") {
					add(engine, "host:"+port)
				}
			}
		}
	}

	// 2. Running Docker containers off a DB image that ARE NOT ThalesOps-managed
	//    (our managed DBs carry the thalesops.db label, so they're excluded).
	if out, err := exec.CommandContext(ctx, "docker", "ps",
		"--format", "{{.Image}}|{{.Names}}|{{.Label \"thalesops.db\"}}").Output(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			parts := strings.SplitN(line, "|", 3)
			if len(parts) < 3 {
				continue
			}
			image, name, label := parts[0], parts[1], parts[2]
			if strings.TrimSpace(label) != "" {
				continue // ours — skip
			}
			for marker, engine := range dbImageEngines {
				if strings.Contains(strings.ToLower(image), marker) {
					add(engine, "container:"+name)
				}
			}
		}
	}

	return found
}

// DetectedDatabasesWire converts findings to the generic shape the heartbeat
// JSON carries.
func DetectedDatabasesWire() []map[string]string {
	dbs := DetectDatabases()
	wire := make([]map[string]string, 0, len(dbs))
	for _, d := range dbs {
		wire = append(wire, map[string]string{"engine": d.Engine, "source": d.Source})
	}
	return wire
}

// ManagedDatabaseHealthWire probes each ThalesOps-managed database container
// (labeled thalesops.db=<id>) and reports whether it's actually accepting
// connections right now — so the dashboard's "RUNNING" reflects live state, not
// just "provisioned once". Reported each heartbeat: [{database_id, status}].
func ManagedDatabaseHealthWire() []map[string]string {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	// id | container-name | image (running containers only)
	out, err := exec.CommandContext(ctx, "docker", "ps",
		"--filter", "label=thalesops.db",
		"--format", "{{.Label \"thalesops.db\"}}|{{.Names}}|{{.Image}}").Output()
	if err != nil {
		return nil
	}

	var health []map[string]string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 3 || strings.TrimSpace(parts[0]) == "" {
			continue
		}
		id, name, image := parts[0], parts[1], strings.ToLower(parts[2])
		status := "STOPPED"
		if managedDBProbe(ctx, name, image) {
			status = "RUNNING"
		}
		health = append(health, map[string]string{"database_id": id, "status": status})
	}
	return health
}

// managedDBProbe runs the engine's readiness check inside the container.
//
// Crucially it probes with the container's OWN credentials (read from its env)
// rather than the default OS user — a bare `pg_isready` defaults to user "root",
// which isn't a Postgres role, so Postgres logs a FATAL on every 60s heartbeat.
// Using the real user keeps the check silent in the database's own logs.
func managedDBProbe(ctx context.Context, name, image string) bool {
	var probe []string
	switch {
	case strings.Contains(image, "redis"):
		probe = []string{"exec", name, "redis-cli", "ping"}
	case strings.Contains(image, "mysql"), strings.Contains(image, "mariadb"):
		probe = []string{"exec", name, "sh", "-c", `mysqladmin ping -u root -p"$MYSQL_ROOT_PASSWORD" 2>/dev/null`}
	default: // postgres
		probe = []string{"exec", name, "sh", "-c", `pg_isready -U "$POSTGRES_USER" -d "$POSTGRES_DB" -q`}
	}
	return exec.CommandContext(ctx, "docker", probe...).Run() == nil
}

// DetectExternalContainers returns running Docker containers that are NOT
// managed by ThalesOps (identified by the lack of our labels).
func DetectExternalContainers(ctx context.Context) []map[string]interface{} {
	var found []map[string]interface{}
	
	// Format: Names|Image|Ports|Status|Label app|Label db
	out, err := exec.CommandContext(ctx, "docker", "ps", 
		"--format", "{{.Names}}|{{.Image}}|{{.Ports}}|{{.Status}}|{{.Label \"thalesops.app\"}}|{{.Label \"thalesops.db\"}}").Output()
	if err != nil {
		return found
	}

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.Split(line, "|")
		if len(parts) < 6 {
			continue
		}
		name, image, ports, status, appLabel, dbLabel := parts[0], parts[1], parts[2], parts[3], parts[4], parts[5]
		if strings.TrimSpace(appLabel) != "" || strings.TrimSpace(dbLabel) != "" {
			continue // skip our managed containers
		}
		if name == "" {
			continue
		}

		found = append(found, map[string]interface{}{
			"source":     "DOCKER",
			"identifier": name,
			"image":      image,
			"ports":      ports,
			"status":     "RUNNING", // if it's in `docker ps`, it's running/paused
			"details": map[string]interface{}{
				"status_string": status,
			},
		})
	}
	return found
}

// DetectSystemdDeployments scans for user-created systemd services that have
// open network listening sockets, excluding distro packages.
func DetectSystemdDeployments(ctx context.Context) []map[string]interface{} {
	var found []map[string]interface{}

	// 1. Get PIDs listening on TCP ports
	out, err := exec.CommandContext(ctx, "ss", "-tlnp").Output()
	if err != nil {
		return found
	}
	
	listenerPIDs := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		// example: ... users:(("node",pid=12345,fd=18))
		if strings.Contains(line, "pid=") {
			// Extract PID naive parsing
			parts := strings.Split(line, "pid=")
			if len(parts) > 1 {
				pidPart := strings.Split(parts[1], ",")[0]
				listenerPIDs[pidPart] = true
			}
		}
	}

	if len(listenerPIDs) == 0 {
		return found
	}

	// 2. Get all active service units
	out, err = exec.CommandContext(ctx, "systemctl", "list-units", "--type=service", "--state=active", "--no-legend").Output()
	if err != nil {
		return found
	}

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		unit := fields[0]

		// 3. For each unit, check if it's user-created (/etc/systemd/system/) and its MainPID matches a listener
		showOut, err := exec.CommandContext(ctx, "systemctl", "show", "-p", "FragmentPath,MainPID,Description", unit).Output()
		if err != nil {
			continue
		}
		
		isUser := false
		mainPID := ""
		desc := ""
		for _, sLine := range strings.Split(strings.TrimSpace(string(showOut)), "\n") {
			if strings.HasPrefix(sLine, "FragmentPath=/etc/systemd/system/") {
				isUser = true
			}
			if strings.HasPrefix(sLine, "MainPID=") {
				mainPID = strings.TrimPrefix(sLine, "MainPID=")
			}
			if strings.HasPrefix(sLine, "Description=") {
				desc = strings.TrimPrefix(sLine, "Description=")
			}
		}

		if isUser && mainPID != "" && mainPID != "0" && listenerPIDs[mainPID] {
			found = append(found, map[string]interface{}{
				"source":      "SYSTEMD",
				"identifier":  unit,
				"description": desc,
				"status":      "RUNNING",
				"main_pid":    mainPID,
			})
		}
	}

	return found
}

// DetectedServicesWire gathers all external deployments (Docker & Systemd)
// to ship in the heartbeat.
func DetectedServicesWire() []map[string]interface{} {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var all []map[string]interface{}
	all = append(all, DetectExternalContainers(ctx)...)
	all = append(all, DetectSystemdDeployments(ctx)...)
	return all
}
