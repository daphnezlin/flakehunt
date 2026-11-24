package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

// DockerRunner runs each trial in a fresh container.
type DockerRunner struct {
	cli     *client.Client
	image   string
	outDir  string   // abs path, one subfolder per trial mounted at /results
	env     []string // passed to every trial
	timeout time.Duration
	slots   chan string // free cpusets, nil if pinning is off
}

func NewDockerRunner(ctx context.Context, image, outDir string, env []string, workers int, timeout time.Duration) (*DockerRunner, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("connect to docker: %w", err)
	}
	abs, err := filepath.Abs(outDir)
	if err != nil {
		return nil, err
	}
	d := &DockerRunner{cli: cli, image: image, outDir: abs, env: env, timeout: timeout}

	// pin each worker to its own cores so trials don't slow each other down.
	// NCPU is what the docker VM has, which can be less than the host.
	info, err := cli.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("docker info: %w", err)
	}
	sets := CPUSets(info.NCPU, workers)
	if sets == nil {
		fmt.Fprintf(os.Stderr, "warning: %d workers but docker only has %d cpus, not pinning (results will be noisier)\n",
			workers, info.NCPU)
	} else {
		d.slots = make(chan string, len(sets))
		for _, s := range sets {
			d.slots <- s
		}
	}
	return d, nil
}

func (d *DockerRunner) Run(ctx context.Context, t Trial) TrialResult {
	res := TrialResult{Trial: t}
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	// never blocks: there's one slot per worker and at most that many trials at once
	if d.slots != nil {
		res.CPUSet = <-d.slots
		defer func() { d.slots <- res.CPUSet }()
	}

	trialDir := filepath.Join(d.outDir, fmt.Sprintf("trial-%04d", t.ID))
	if err := os.MkdirAll(trialDir, 0o777); err != nil {
		res.Err = err
		return res
	}
	// container might not run as us
	_ = os.Chmod(trialDir, 0o777)

	cmd := PytestArgs(t, "/results/junit.xml")

	hostCfg := &container.HostConfig{Binds: []string{trialDir + ":/results"}}
	hostCfg.Resources.CpusetCpus = res.CPUSet
	if t.CPUs > 0 {
		hostCfg.Resources.NanoCPUs = int64(t.CPUs * 1e9)
	}

	created, err := d.cli.ContainerCreate(ctx,
		&container.Config{Image: d.image, Cmd: cmd, Env: d.env},
		hostCfg, nil, nil, "")
	if err != nil {
		res.Err = fmt.Errorf("create container: %w", err)
		return res
	}
	// use Background so cleanup still happens after cancel
	defer d.cli.ContainerRemove(context.Background(), created.ID, container.RemoveOptions{Force: true})

	if err := d.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		res.Err = fmt.Errorf("start container: %w", err)
		return res
	}

	statusCh, errCh := d.cli.ContainerWait(ctx, created.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if errors.Is(err, context.DeadlineExceeded) {
			res.TimedOut = true
		} else {
			res.Err = fmt.Errorf("wait for container: %w", err)
		}
		return res
	case <-statusCh:
		// pytest exits 1 when tests fail, that's fine. only care if junit.xml exists
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
