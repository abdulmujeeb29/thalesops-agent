package executor

import (
	"strings"
	"testing"

	"github.com/thalesops/agent/internal/models"
)

func argsString(args []string) string { return strings.Join(args, " ") }

// A managed Redis must never be able to grow until the host OOMs and the kernel
// kills the user's app instead.
func TestRedisRunArgsAreCapped(t *testing.T) {
	args, dataDir := dbRunArgs(models.DatabasePayload{
		Engine: "REDIS", Image: "redis", Version: "7",
		ContainerName: "thalesops-db-abc", VolumeName: "thalesops-dbvol-abc",
		NetworkName: "thalesops-dbnet-abc", DatabaseID: "abc",
	})
	joined := argsString(args)

	if !strings.Contains(joined, "--memory") {
		t.Error("Redis container must have a Docker memory limit")
	}
	if !strings.Contains(joined, "--maxmemory ") {
		t.Error("Redis must be given its own maxmemory ceiling")
	}
	if !strings.Contains(joined, "--maxmemory-policy noeviction") {
		t.Error("policy must be noeviction so a queue never silently loses jobs")
	}
	if !strings.Contains(joined, "--appendonly yes") {
		t.Error("AOF must be on so a crash doesn't lose writes")
	}
	if dataDir != "/data" {
		t.Errorf("expected /data, got %s", dataDir)
	}
}

// Args after the image replace the image's CMD, so `redis-server` has to be named
// explicitly or the container starts with no server at all.
func TestRedisServerCommandFollowsImage(t *testing.T) {
	args, _ := dbRunArgs(models.DatabasePayload{
		Engine: "REDIS", Image: "redis", Version: "7",
		ContainerName: "c", VolumeName: "v", NetworkName: "n", DatabaseID: "id",
	})

	imageIdx, serverIdx := -1, -1
	for i, a := range args {
		if a == "redis:7" {
			imageIdx = i
		}
		if a == "redis-server" {
			serverIdx = i
		}
	}
	if imageIdx < 0 {
		t.Fatal("image missing from run args")
	}
	if serverIdx != imageIdx+1 {
		t.Fatalf("redis-server must come immediately after the image; got %v", args)
	}
}

// Redis must hit its own ceiling before Docker kills the container, so the
// failure surfaces as a write error rather than a vanished database.
func TestRedisCapLeavesHeadroomUnderDockerLimit(t *testing.T) {
	dockerCap, redisCap := redisMemoryCapsMB()

	if redisCap >= dockerCap {
		t.Errorf("redis cap (%d) must sit below the docker cap (%d)", redisCap, dockerCap)
	}
	if dockerCap < redisMinCapMB || dockerCap > redisMaxCapMB {
		t.Errorf("docker cap %d outside the clamp [%d, %d]", dockerCap, redisMinCapMB, redisMaxCapMB)
	}
	if redisCap <= 0 {
		t.Errorf("redis cap must be positive, got %d", redisCap)
	}
}

// The cap change is Redis-only on purpose: Postgres and MySQL manage their own
// memory and already work, so they keep exactly the behaviour they had.
func TestOtherEnginesAreUnchanged(t *testing.T) {
	for _, engine := range []string{"POSTGRES", "MYSQL"} {
		args, _ := dbRunArgs(models.DatabasePayload{
			Engine: engine, Image: strings.ToLower(engine), Version: "1",
			ContainerName: "c", VolumeName: "v", NetworkName: "n", DatabaseID: "id",
		})
		if strings.Contains(argsString(args), "--memory") {
			t.Errorf("%s should not have gained a memory limit", engine)
		}
	}
}

// No engine may publish a host port — a managed database is reachable only over
// its private network, which is why it can safely run without a password.
func TestNoEnginePublishesAPort(t *testing.T) {
	for _, engine := range []string{"REDIS", "POSTGRES", "MYSQL"} {
		args, _ := dbRunArgs(models.DatabasePayload{
			Engine: engine, Image: "img", Version: "1",
			ContainerName: "c", VolumeName: "v", NetworkName: "n", DatabaseID: "id",
		})
		for _, a := range args {
			if a == "-p" || a == "--publish" {
				t.Errorf("%s must not publish a host port", engine)
			}
		}
	}
}
