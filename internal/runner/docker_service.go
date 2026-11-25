package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
)

const (
	proxyListen   = "0.0.0.0:8666"
	proxyAddr     = "toxiproxy:8666"
	toxiproxyPort = "8474/tcp"
)

var labels = map[string]string{"flakehunt": "1"} // so leftovers are easy to find and clean up

// ensureImage pulls an image if it isn't already local.
func ensureImage(ctx context.Context, cli *client.Client, ref string) error {
	if _, err := cli.ImageInspect(ctx, ref); err == nil {
		return nil
	} else if !client.IsErrNotFound(err) {
		return err
	}
	fmt.Fprintf(os.Stderr, "pulling %s\n", ref)
	rc, err := cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	defer rc.Close()
	_, err = io.Copy(io.Discard, rc) // pull isn't done until the stream ends
	return err
}

// startService sets up the network, service and toxiproxy for one trial and
// returns the extra env for the test container and a cleanup func. cleanup is
// safe to call even when err != nil.
func (d *DockerRunner) startService(ctx context.Context, t Trial) (netName string, env []string, cleanup func(), err error) {
	var containers []string
	netName = fmt.Sprintf("flakehunt-%d-%d", os.Getpid(), t.ID)
	netCreated := false
	cleanup = func() {
		bg := context.Background()
		for _, id := range containers {
			d.cli.ContainerRemove(bg, id, container.RemoveOptions{Force: true})
		}
		if netCreated {
			d.cli.NetworkRemove(bg, netName)
		}
	}

	if _, err = d.cli.NetworkCreate(ctx, netName, network.CreateOptions{Driver: "bridge", Labels: labels}); err != nil {
		return "", nil, cleanup, fmt.Errorf("create network: %w", err)
	}
	netCreated = true
	onNet := func(alias string) *network.NetworkingConfig {
		return &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
			netName: {Aliases: []string{alias}},
		}}
	}

	svcCfg := &container.Config{Image: d.service.Image, Cmd: d.service.Cmd, Labels: labels}
	if d.service.Health != "" {
		svcCfg.Healthcheck = &container.HealthConfig{
			Test:     []string{"CMD-SHELL", d.service.Health},
			Interval: 250 * time.Millisecond,
			Timeout:  2 * time.Second,
			Retries:  120,
		}
	}
	mode := container.NetworkMode(netName)
	svc, err := d.cli.ContainerCreate(ctx, svcCfg, &container.HostConfig{NetworkMode: mode}, onNet("service"), nil, "")
	if err != nil {
		return "", nil, cleanup, fmt.Errorf("create service: %w", err)
	}
	containers = append(containers, svc.ID)

	// with no latency there's nothing for toxiproxy to do, so tests talk to the
	// service directly and the trial needs one less container. toxiproxy adds
	// no delay of its own worth measuring, so it's the same condition
	addr := "service:" + strconv.Itoa(d.service.Port)
	var proxyID string
	if t.LatencyMS > 0 {
		// publish the api port on a random localhost port so flakehunt can configure it
		proxy, err := d.cli.ContainerCreate(ctx,
			&container.Config{Image: d.toxiproxyImage, Labels: labels, ExposedPorts: nat.PortSet{toxiproxyPort: {}}},
			&container.HostConfig{NetworkMode: mode, PortBindings: nat.PortMap{toxiproxyPort: {{HostIP: "127.0.0.1"}}}},
			onNet("toxiproxy"), nil, "")
		if err != nil {
			return "", nil, cleanup, fmt.Errorf("create toxiproxy: %w", err)
		}
		containers = append(containers, proxy.ID)
		proxyID = proxy.ID
		addr = proxyAddr
	}

	for _, id := range containers {
		if err = d.cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
			return "", nil, cleanup, fmt.Errorf("start service containers: %w", err)
		}
	}

	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if d.service.Health != "" {
		if err = d.waitHealthy(readyCtx, svc.ID); err != nil {
			return "", nil, cleanup, err
		}
	}

	if proxyID != "" {
		info, err := d.cli.ContainerInspect(ctx, proxyID)
		if err != nil {
			return "", nil, cleanup, err
		}
		bindings := info.NetworkSettings.Ports[toxiproxyPort]
		if len(bindings) == 0 {
			return "", nil, cleanup, fmt.Errorf("toxiproxy api port wasn't published")
		}
		tp := newToxiproxyClient("http://127.0.0.1:" + bindings[0].HostPort)
		if err = tp.waitReady(readyCtx); err != nil {
			return "", nil, cleanup, err
		}
		if err = tp.createProxy(ctx, "service", proxyListen, "service:"+strconv.Itoa(d.service.Port)); err != nil {
			return "", nil, cleanup, err
		}
		if err = tp.addLatency(ctx, "service", t.LatencyMS, Jitter(t.LatencyMS)); err != nil {
			return "", nil, cleanup, err
		}
	}

	for _, e := range d.service.Env {
		env = append(env, strings.ReplaceAll(e, "{addr}", addr))
	}
	return netName, env, cleanup, nil
}

// waitHealthy waits for docker's healthcheck to pass, same idea as compose's
// depends_on: condition: service_healthy. Without it tests could start before
// the service is listening and fail for a reason that has nothing to do with them.
func (d *DockerRunner) waitHealthy(ctx context.Context, id string) error {
	for {
		info, err := d.cli.ContainerInspect(ctx, id)
		if err != nil {
			return err
		}
		if h := info.State.Health; h != nil {
			switch h.Status {
			case container.Healthy:
				return nil
			case container.Unhealthy:
				return fmt.Errorf("service failed its health check")
			}
		}
		if !info.State.Running {
			return fmt.Errorf("service exited (code %d)", info.State.ExitCode)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("service never became healthy: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}
