// Copyright 2019 Drone.IO Inc. All rights reserved.
// Use of this source code is governed by the Polyform License
// that can be found in the LICENSE file.

package engine

import (
	"context"
	"io"
	"io/ioutil"

	"github.com/drone-runners/drone-runner-docker/engine/stdcopy"
	"github.com/drone/drone-runtime/engine/docker/auth"

	"docker.io/go-docker"
	"docker.io/go-docker/api/types"
	"docker.io/go-docker/api/types/volume"
)

type engine struct {
	client docker.APIClient
}

// New returns a new engine.
func New(client docker.APIClient) Engine {
	return &engine{client}
}

// NewEnv returns a new Engine from the environment.
func NewEnv() (Engine, error) {
	cli, err := docker.NewEnvClient()
	if err != nil {
		return nil, err
	}
	return New(cli), nil
}

// Setup the pipeline environment.
func (e *engine) Setup(ctx context.Context, spec *Spec) error {
	// creates the default temporary (local) volumes
	// that are mounted into each container step.
	for _, vol := range spec.Volumes {
		if vol.EmptyDir == nil {
			continue
		}
		_, err := e.client.VolumeCreate(ctx, volume.VolumesCreateBody{
			Name:   vol.EmptyDir.ID,
			Driver: "local",
			Labels: vol.EmptyDir.Labels,
		})
		if err != nil {
			return err
		}
	}

	// creates the default pod network. All containers
	// defined in the pipeline are attached to this network.
	driver := "bridge"
	if spec.Platform.OS == "windows" {
		driver = "nat"
	}
	_, err := e.client.NetworkCreate(ctx, spec.Network.ID, types.NetworkCreate{
		Driver: driver,
		Labels: spec.Network.Labels,
	})

	return err
}

// Destroy the pipeline environment.
func (e *engine) Destroy(ctx context.Context, spec *Spec) error {
	removeOpts := types.ContainerRemoveOptions{
		Force:         true,
		RemoveLinks:   false,
		RemoveVolumes: true,
	}

	// stop all containers
	for _, step := range spec.Steps {
		e.client.ContainerKill(ctx, step.ID, "9")
	}

	// cleanup all containers
	for _, step := range spec.Steps {
		e.client.ContainerRemove(ctx, step.ID, removeOpts)
	}

	// cleanup all volumes
	for _, vol := range spec.Volumes {
		if vol.EmptyDir == nil {
			continue
		}
		// tempfs volumes do not have a volume entry,
		// and therefore do not require removal.
		if vol.EmptyDir.Medium == "memory" {
			continue
		}
		e.client.VolumeRemove(ctx, vol.EmptyDir.ID, true)
	}

	// cleanup the network
	e.client.NetworkRemove(ctx, spec.Network.ID)

	// notice that we never collect or return any errors.
	// this is because we silently ignore cleanup failures
	// and instead ask the system admin to periodically run
	// `docker prune` commands.
	return nil
}

// Run runs the pipeline step.
func (e *engine) Run(ctx context.Context, spec *Spec, step *Step, output io.Writer) (*State, error) {
	// create the container
	err := e.create(ctx, spec, step, output)
	if err != nil {
		return nil, err
	}
	// start the container
	err = e.start(ctx, step.ID)
	if err != nil {
		return nil, err
	}
	// tail the container
	err = e.tail(ctx, step.ID, output)
	if err != nil {
		return nil, err
	}
	// wait for the response
	return e.wait(ctx, step.ID)
}

//
// emulate docker commands
//

func (e *engine) create(ctx context.Context, spec *Spec, step *Step, output io.Writer) error {
	// parse the docker image name. We need to extract the
	// image domain name and match to registry credentials
	// stored in the .docker/config.json object.
	_, _, latest, err := parseImage(step.Image)
	if err != nil {
		return err
	}

	// create pull options with encoded authorization credentials.
	pullopts := types.ImagePullOptions{}
	if step.Auth != nil {
		pullopts.RegistryAuth = auth.Encode(
			step.Auth.Username,
			step.Auth.Password,
		)
	}

	// automatically pull the latest version of the image if requested
	// by the process configuration, or if the image is :latest
	if step.Pull == PullAlways ||
		(step.Pull == PullDefault && latest) {
		rc, pullerr := e.client.ImagePull(ctx, step.Image, pullopts)
		if pullerr == nil {
			io.Copy(ioutil.Discard, rc)
			rc.Close()
		}
		if pullerr != nil {
			return pullerr
		}
	}

	_, err = e.client.ContainerCreate(ctx,
		toConfig(spec, step),
		toHostConfig(spec, step),
		toNetConfig(spec, step),
		step.ID,
	)

	// automatically pull and try to re-create the image if the
	// failure is caused because the image does not exist.
	if docker.IsErrImageNotFound(err) && step.Pull != PullNever {
		rc, pullerr := e.client.ImagePull(ctx, step.Image, pullopts)
		if pullerr != nil {
			return pullerr
		}
		io.Copy(ioutil.Discard, rc)
		rc.Close()

		// once the image is successfully pulled we attempt to
		// re-create the container.
		_, err = e.client.ContainerCreate(ctx,
			toConfig(spec, step),
			toHostConfig(spec, step),
			toNetConfig(spec, step),
			step.ID,
		)
	}
	if err != nil {
		return err
	}

	// // use the default user-defined network if network_mode
	// // is not otherwise specified.
	// if step.Network == "" {
	// 	for _, net := range step.Networks {
	// 		err = e.client.NetworkConnect(ctx, net, step.ID, &network.EndpointSettings{
	// 			Aliases: []string{net},
	// 		})
	// 		if err != nil {
	// 			return nil
	// 		}
	// 	}
	// }

	return nil
}

// helper function emulates the `docker start` command.
func (e *engine) start(ctx context.Context, id string) error {
	return e.client.ContainerStart(ctx, id, types.ContainerStartOptions{})
}

// helper function emulates the `docker wait` command, blocking
// until the container stops and returning the exit code.
func (e *engine) wait(ctx context.Context, id string) (*State, error) {
	wait, errc := e.client.ContainerWait(ctx, id, "")
	select {
	case <-wait:
	case <-errc:
	}

	info, err := e.client.ContainerInspect(ctx, id)
	if err != nil {
		return nil, err
	}
	if info.State.Running {
		// TODO(bradrydewski) if the state is still running
		// we should call wait again.
	}

	return &State{
		Exited:    true,
		ExitCode:  info.State.ExitCode,
		OOMKilled: info.State.OOMKilled,
	}, nil
}

// helper function emulates the `docker logs -f` command, streaming
// all container logs until the container stops.
func (e *engine) tail(ctx context.Context, id string, output io.Writer) error {
	opts := types.ContainerLogsOptions{
		Follow:     true,
		ShowStdout: true,
		ShowStderr: true,
		Details:    false,
		Timestamps: false,
	}

	logs, err := e.client.ContainerLogs(ctx, id, opts)
	if err != nil {
		return err
	}

	go func() {
		stdcopy.StdCopy(output, output, logs)
		logs.Close()
	}()
	return nil
}