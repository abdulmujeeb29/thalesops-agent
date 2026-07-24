package executor

import (
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/thalesops/agent/internal/models"
)

// Managed databases are persistent, network-isolated database containers.
// Data lives in a named Docker volume (survives restarts/redeploys); the DB is
// reachable only over a private per-database Docker network (never a public
// port); apps that attach join that network and reach the DB by container name.

// ExecuteProvisionDB creates the volume + network and runs the database
// container, then waits until it actually accepts connections.
func ExecuteProvisionDB(rawPayload map[string]interface{}, timeout time.Duration, flush FlushFunc) models.CommandResultRequest {
	p := parseDatabasePayload(rawPayload)

	sh := NewLogShipper(flush, dbSecrets(p))
	defer sh.Close()

	fail := func(msg string) models.CommandResultRequest {
		sh.Write("stderr", msg)
		return models.CommandResultRequest{ExitCode: 1, Stderr: msg}
	}

	if p.ContainerName == "" || p.Image == "" {
		return fail("provision-db: incomplete payload")
	}
	if err := system_EnsureDocker(); err != nil {
		return fail(err.Error())
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	sh.System(fmt.Sprintf("Provisioning %s %s…", p.Engine, p.Version))

	// Volume (persists data across container/agent restarts) — idempotent.
	_ = exec.CommandContext(ctx, "docker", "volume", "create", p.VolumeName).Run()
	// Private network the app + DB share — ignore "already exists".
	_ = exec.CommandContext(ctx, "docker", "network", "create", p.NetworkName).Run()

	// Replace any half-provisioned container from a previous attempt.
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", p.ContainerName).Run()

	args, dataDir := dbRunArgs(p)
	sh.System("Starting the database container…")
	if code, err := runStreaming(ctx, sh, "docker", args...); err != nil {
		return models.CommandResultRequest{ExitCode: code, Stderr: "could not start database: " + err.Error()}
	}
	_ = dataDir

	sh.System("Waiting for the database to accept connections…")
	if err := waitForDBReady(ctx, sh, p); err != nil {
		return fail(err.Error())
	}

	sh.System("Database is ready.")
	return models.CommandResultRequest{ExitCode: 0, Stdout: "provisioned " + p.ContainerName}
}

// ExecuteDeleteDB removes the database container, and (unless the data is being
// kept) its volume and network.
func ExecuteDeleteDB(rawPayload map[string]interface{}, timeout time.Duration, flush FlushFunc) models.CommandResultRequest {
	p := parseDatabasePayload(rawPayload)

	sh := NewLogShipper(flush, nil)
	defer sh.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	sh.System("Removing the database container…")
	_ = exec.CommandContext(ctx, "docker", "rm", "-f", p.ContainerName).Run()

	if p.DestroyData {
		sh.System("Destroying the data volume…")
		_ = exec.CommandContext(ctx, "docker", "volume", "rm", p.VolumeName).Run()
	} else {
		sh.System("Keeping the data volume (" + p.VolumeName + ").")
	}
	// Best-effort — fails harmlessly if another container still uses it.
	_ = exec.CommandContext(ctx, "docker", "network", "rm", p.NetworkName).Run()

	return models.CommandResultRequest{ExitCode: 0, Stdout: "deleted " + p.ContainerName}
}

// dbRunArgs builds the `docker run` args per engine. Returns the args and the
// in-container data directory (for reference).
func dbRunArgs(p models.DatabasePayload) ([]string, string) {
	base := []string{"run", "-d", "--name", p.ContainerName,
		"--network", p.NetworkName,
		"--label", "thalesops.db=" + p.DatabaseID,
		"--restart", "unless-stopped"}

	image := p.Image + ":" + p.Version
	switch p.Engine {
	case "REDIS":
		return append(base, "-v", p.VolumeName+":/data", image), "/data"
	case "MYSQL":
		return append(base,
			"-e", "MYSQL_ROOT_PASSWORD="+p.DBPassword,
			"-e", "MYSQL_DATABASE="+p.DBName,
			"-e", "MYSQL_USER="+p.DBUser,
			"-e", "MYSQL_PASSWORD="+p.DBPassword,
			"-v", p.VolumeName+":/var/lib/mysql", image), "/var/lib/mysql"
	default: // POSTGRES
		return append(base,
			"-e", "POSTGRES_USER="+p.DBUser,
			"-e", "POSTGRES_PASSWORD="+p.DBPassword,
			"-e", "POSTGRES_DB="+p.DBName,
			"-v", p.VolumeName+":/var/lib/postgresql/data", image), "/var/lib/postgresql/data"
	}
}

// waitForDBReady polls the engine's own readiness probe inside the container
// (no published port needed) until it responds or the deadline passes.
func waitForDBReady(ctx context.Context, sh *LogShipper, p models.DatabasePayload) error {
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if !dbContainerRunning(ctx, p.ContainerName) {
			dumpCheckLogs(ctx, sh, p.ContainerName)
			return fmt.Errorf("the database container exited during startup")
		}
		if dbProbe(ctx, p) {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	dumpCheckLogs(ctx, sh, p.ContainerName)
	return fmt.Errorf("the database did not become ready within 90s")
}

func dbProbe(ctx context.Context, p models.DatabasePayload) bool {
	var probe []string
	switch p.Engine {
	case "REDIS":
		probe = []string{"exec", p.ContainerName, "redis-cli", "ping"}
	case "MYSQL":
		probe = []string{"exec", p.ContainerName, "mysqladmin", "ping", "-h", "127.0.0.1",
			"-u", "root", "-p" + p.DBPassword}
	default: // POSTGRES
		probe = []string{"exec", p.ContainerName, "pg_isready", "-U", p.DBUser}
	}
	return exec.CommandContext(ctx, "docker", probe...).Run() == nil
}

func dbContainerRunning(ctx context.Context, name string) bool {
	out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Running}}", name).Output()
	return err == nil && string(out) == "true\n"
}

func parseDatabasePayload(m map[string]interface{}) models.DatabasePayload {
	return models.DatabasePayload{
		DatabaseID:    asString(m["database_id"]),
		Engine:        asString(m["engine"]),
		Image:         asString(m["image"]),
		Version:       asString(m["version"]),
		ContainerName: asString(m["container_name"]),
		VolumeName:    asString(m["volume_name"]),
		NetworkName:   asString(m["network_name"]),
		InternalPort:  asInt(m["internal_port"]),
		DBUser:        asString(m["db_user"]),
		DBName:        asString(m["db_name"]),
		DBPassword:    asString(m["db_password"]),
		DestroyData:   asBool(m["destroy_data"]),
	}
}

// dbSecrets keeps the generated password out of streamed logs.
func dbSecrets(p models.DatabasePayload) []string {
	if len(p.DBPassword) >= 6 {
		return []string{p.DBPassword}
	}
	return nil
}

// system_EnsureDocker is a thin wrapper so provisioning fails clearly if Docker
// isn't available (deploys already guard this via EnsureDeployPrerequisites).
func system_EnsureDocker() error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker is not installed on this server")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		return fmt.Errorf("docker daemon is not reachable")
	}
	return nil
}
