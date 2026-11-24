package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// DockerRunner runs each trial in a fresh container on the local Docker engine.
type DockerRunner struct {
	cli     *client.Client
	image   string
	outDir  string   // absolute path; each trial gets its own subfolder mounted at /results
	env     []string // extra env vars passed to every trial, e.g. FLAKY_ZOO_RACE_WAIT=0.02
	cpus    float64       // 0 = no limit
	timeout time.Duration // per trial; a test suite that hangs past this counts as a timeout
}

func NewDockerRunner(image, outDir string, env []string, cpus float64, timeout time.Duration) (*DockerRunner, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("connect to docker: %w", err)
	}
	abs, err := filepath.Abs(outDir)
	if err != nil {
		return nil, err
	}
	return &DockerRunner{cli: cli, image: image, outDir: abs, env: env, cpus: cpus, timeout: timeout}, nil
}

func (d *DockerRunner) Run(ctx context.Context, t Trial) TrialResult {
	res := TrialResult{Trial: t}
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	trialDir := filepath.Join(d.outDir, fmt.Sprintf("trial-%04d", t.ID))
	if err := os.MkdirAll(trialDir, 0o777); err != nil {
		res.Err = err
		return res
	}
	// Containers often run as root (or as a different user): make the folder writable to them.
	_ = os.Chmod(trialDir, 0o777)

	cmd := []string{
		"pytest", "-q", "-p", "no:cacheprovider",
		"--randomly-seed=" + strconv.FormatInt(t.Seed, 10),
		"--junitxml=/results/junit.xml",
	}

	hostCfg := &container.HostConfig{Binds: []string{trialDir + ":/results"}}
	if d.cpus > 0 {
		hostCfg.Resources.NanoCPUs = int64(d.cpus * 1e9)
	}

	created, err := d.cli.ContainerCreate(ctx,
		&container.Config{Image: d.image, Cmd: cmd, Env: d.env},
		hostCfg, nil, nil, "")
	if err != nil {
		res.Err = fmt.Errorf("create container: %w", err)
		return res
	}
	// Always clean up, even if ctx was cancelled, so trials don't leak containers.
	defer d.cli.ContainerRemove(context.Background(), created.ID, container.RemoveOptions{Force: true})

	if err := d.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		res.Err = fmt.Errorf("start container: %w", err)
		return res
	}

	statusCh, errCh := d.cli.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if errors.Is(err, context.DeadlineExceeded) {
			res.TimedOut = true // a hang is its own kind of flakiness
		} else {
			res.Err = fmt.Errorf("wait for container: %w", err)
		}
		return res
	case <-statusCh:
		// pytest exits non-zero when tests fail; that's data, not an error.
		// What matters is whether it produced a results file.
	}

	f, err := os.Open(filepath.Join(trialDir, "junit.xml"))
	if err != nil {
		res.Err = fmt.Errorf("no results file (did pytest crash?): %w", err)
		return res
	}
	defer f.Close()
	res.Tests, res.Err = ParseJUnit(f)
	return res
}
