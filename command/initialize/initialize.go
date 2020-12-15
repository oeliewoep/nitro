package initialize

import (
	"bytes"
	"fmt"
	"os"
	"strconv"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	volumetypes "github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/spf13/cobra"

	"github.com/craftcms/nitro/command/version"
	"github.com/craftcms/nitro/labels"
	"github.com/craftcms/nitro/terminal"
)

const exampleText = `  # create a new environment with the default environment
  nitro init

  # create a new environment overriding the default name
  nitro init --environment my-new-env

  # you can override the environment by setting the variable "NITRO_DEFAULT_ENVIRONMENT"`

// NewCommnad takes a docker client and returns the init command for creating a new environment
func NewCommand(docker client.CommonAPIClient, output terminal.Outputer) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "init",
		Short:   "Create new environment",
		Example: exampleText,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			env := cmd.Flag("environment").Value.String()

			output.Info(fmt.Sprintf("Checking %s...", env))

			// create filters for the development environment
			filter := filters.NewArgs()
			filter.Add("name", env)

			// check if the network needs to be created
			networks, err := docker.NetworkList(ctx, types.NetworkListOptions{Filters: filter})
			if err != nil {
				return fmt.Errorf("unable to list the docker networks, %w", err)
			}

			// since the filter is fuzzy, do an exact match (e.g. filtering for
			// `nitro-dev` will also return `nitro-dev-host`
			var skipNetwork bool
			var networkID string
			for _, n := range networks {
				if n.Name == env {
					skipNetwork = true
					networkID = n.ID
				}
			}

			// create the network needs to be created
			switch skipNetwork {
			case true:
				output.Success("network ready")
			default:
				output.Pending("creating network")

				resp, err := docker.NetworkCreate(ctx, env, types.NetworkCreate{
					Driver:     "bridge",
					Attachable: true,
					Labels: map[string]string{
						labels.Environment: env,
						labels.Network:     env,
					},
				})
				if err != nil {
					return fmt.Errorf("unable to create the network, %w", err)
				}

				// set the newly created network
				networkID = resp.ID

				output.Done()
			}

			// check if the volume needs to be created
			volumes, err := docker.VolumeList(ctx, filter)
			if err != nil {
				return fmt.Errorf("unable to list volumes, %w", err)
			}

			// since the filter is fuzzy, do an exact match (e.g. filtering for
			// `nitro-dev` will also return `nitro-dev-host`
			var skipVolume bool
			var volume *types.Volume
			for _, v := range volumes.Volumes {
				if v.Name == env {
					skipVolume = true
					volume = v
				}
			}

			// check if the volume needs to be created
			switch skipVolume {
			case true:
				output.Success("volume ready")
			default:
				output.Pending("creating volume")

				// create a volume with the same name of the machine
				resp, err := docker.VolumeCreate(ctx, volumetypes.VolumesCreateBody{
					Driver: "local",
					Name:   env,
					Labels: map[string]string{
						labels.Environment: env,
						labels.Volume:      env,
					},
				})
				if err != nil {
					return fmt.Errorf("unable to create the volume, %w", err)
				}

				volume = &resp

				output.Done()
			}

			// build the proxy image ref
			proxyImage := fmt.Sprintf("craftcms/nitro-proxy:%s", version.Version)

			imageFilter := filters.NewArgs()
			imageFilter.Add("reference", proxyImage)

			// check for the proxy image
			images, err := docker.ImageList(cmd.Context(), types.ImageListOptions{
				Filters: imageFilter,
			})
			if err != nil {
				return fmt.Errorf("unable to get a list of images, %w", err)
			}

			// remove this logic check once published to add a method for developing locally
			if len(images) == 0 {
				output.Pending("pulling image")

				rdr, err := docker.ImagePull(ctx, proxyImage, types.ImagePullOptions{All: false})
				if err != nil {
					return fmt.Errorf("unable to pull the nitro-proxy from docker hub, %w", err)
				}

				buf := &bytes.Buffer{}
				if _, err := buf.ReadFrom(rdr); err != nil {
					return fmt.Errorf("unable to read the output from pulling the image, %w", err)
				}

				output.Done()
			}

			// create a filter for the nitro proxy
			proxyFilter := filters.NewArgs()
			proxyFilter.Add("label", labels.Proxy+"="+env)

			// check if there is an existing container for the nitro-proxy
			var containerID string
			containers, err := docker.ContainerList(ctx, types.ContainerListOptions{Filters: proxyFilter, All: true})
			if err != nil {
				return fmt.Errorf("unable to list the containers\n%w", err)
			}

			var proxyRunning bool
			for _, c := range containers {
				for _, n := range c.Names {
					if n == env || n == "/"+env {
						output.Success("proxy ready")

						containerID = c.ID

						// check if it is running
						if c.State == "running" {
							proxyRunning = true
						}
					}
				}
			}

			// if we do not have a container id, it needs to be create
			if containerID == "" {
				output.Pending("creating proxy")

				// set ports
				var httpPort, httpsPort, apiPort, xdebugPort nat.Port

				// check for a custom HTTP port
				switch os.Getenv("NITRO_HTTP_PORT") {
				case "":
					httpPort, err = nat.NewPort("tcp", "80")
					if err != nil {
						return fmt.Errorf("unable to set the HTTP port, %w", err)
					}
				default:
					if os.Getenv("NITRO_HTTP_PORT") != "" {
						httpPort, err = nat.NewPort("tcp", os.Getenv("NITRO_HTTP_PORT"))
						if err != nil {
							return fmt.Errorf("unable to set the HTTP port, %w", err)
						}
					}
				}

				// check for a custom HTTPS port
				switch os.Getenv("NITRO_HTTPS_PORT") {
				case "":
					httpsPort, err = nat.NewPort("tcp", "443")
					if err != nil {
						return fmt.Errorf("unable to set the HTTPS port, %w", err)
					}
				default:
					if os.Getenv("NITRO_HTTPS_PORT") != "" {
						httpsPort, _ = nat.NewPort("tcp", os.Getenv("NITRO_HTTPS_PORT"))
						if err != nil {
							return fmt.Errorf("unable to set the HTTPS port, %w", err)
						}
					}
				}

				// check for a custom API port
				switch os.Getenv("NITRO_API_PORT") {
				case "":
					apiPort, err = nat.NewPort("tcp", "5000")
					if err != nil {
						return fmt.Errorf("unable to set the API port, %w", err)
					}
				default:
					if os.Getenv("NITRO_API_PORT") != "" {
						apiPort, _ = nat.NewPort("tcp", os.Getenv("NITRO_API_PORT"))
						if err != nil {
							return fmt.Errorf("unable to set the API port, %w", err)
						}
					}
				}

				// check for a custom xdebug port
				switch os.Getenv("NITRO_XDEBUG_PORT") {
				case "":
					xdebugPort, err = nat.NewPort("tcp", "9003")
					if err != nil {
						return fmt.Errorf("unable to set the API port, %w", err)
					}
				default:
					if os.Getenv("NITRO_XDEBUG_PORT") != "" {
						xdebugPort, _ = nat.NewPort("tcp", os.Getenv("NITRO_XDEBUG_PORT"))
						if err != nil {
							return fmt.Errorf("unable to set the xdebug port, %w", err)
						}
					}
				}

				// create a container
				resp, err := docker.ContainerCreate(ctx,
					&container.Config{
						Image: proxyImage,
						ExposedPorts: nat.PortSet{
							httpPort:   struct{}{},
							httpsPort:  struct{}{},
							apiPort:    struct{}{},
							xdebugPort: struct{}{},
						},
						Labels: map[string]string{
							labels.Type:         "proxy",
							labels.Environment:  env,
							labels.Proxy:        env,
							labels.ProxyVersion: version.Version,
						},
					},
					&container.HostConfig{
						NetworkMode: "default",
						Mounts: []mount.Mount{
							{
								Type:   mount.TypeVolume,
								Source: volume.Name,
								Target: "/data",
							},
						},
						PortBindings: map[nat.Port][]nat.PortBinding{
							httpPort: {
								{
									HostIP:   "127.0.0.1",
									HostPort: "80",
								},
							},
							httpsPort: {
								{
									HostIP:   "127.0.0.1",
									HostPort: "443",
								},
							},
							apiPort: {
								{
									HostIP:   "127.0.0.1",
									HostPort: "5000",
								},
							},
							xdebugPort: {
								{
									HostIP:   "127.0.0.1",
									HostPort: "9003",
								},
							},
						},
					},
					&network.NetworkingConfig{
						EndpointsConfig: map[string]*network.EndpointSettings{
							env: {
								NetworkID: networkID,
							},
						},
					},
					env,
				)
				if err != nil {
					return fmt.Errorf("unable to create the container from image %s\n%w", proxyImage, err)
				}

				containerID = resp.ID

				output.Done()
			}

			// start the container for the proxy if its not running
			if !proxyRunning {
				if err := docker.ContainerStart(ctx, containerID, types.ContainerStartOptions{}); err != nil {
					return fmt.Errorf("unable to start the nitro container, %w", err)
				}
			}

			// convert the apply flag to a boolean
			skipApply, err := strconv.ParseBool(cmd.Flag("skip-apply").Value.String())
			if err != nil {
				// don't do anything
			}

			// check if we need to run the
			if skipApply != true && cmd.Parent() != nil {
				// TODO(jasonmccallister) make this better :)
				for _, c := range cmd.Parent().Commands() {
					// set the apply command
					if c.Use == "apply" {
						if err := c.RunE(c, args); err != nil {
							return err
						}
					}

					// set the trust command
					if c.Use == "trust" {
						if err := c.RunE(c, args); err != nil {
							return err
						}
					}
				}
			}

			output.Info(env, "is ready! 🚀")

			return nil
		},
	}

	// set flags for the command
	cmd.Flags().Bool("skip-apply", false, "skip applying changes")

	return cmd
}
