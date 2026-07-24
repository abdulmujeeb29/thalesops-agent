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
