// SPDX-License-Identifier: GPL-3.0-only
//
// This file is part of the podman-launcher project:
//
//	https://github.com/89luca89/podman-launcher
//
// # Copyright (C) 2023 podman-launcher contributors
//
// podman-launcher is free software; you can redistribute it and/or modify it
// under the terms of the GNU General Public License version 3
// as published by the Free Software Foundation.
//
// podman-launcher is distributed in the hope that it will be useful, but
// WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU
// General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with podman-launcher; if not, see <http://www.gnu.org/licenses/>.
package main

import (
	"bytes"
	_ "embed"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"text/template"
)

// The purpose of this program/launcher is only to ship the latest release of
//
//	https://github.com/mgoltzsche/podman-static/
//
// This repo builds and releases all podman components as statically linked binaries
// this will let us to easily ship the container manager without needing all the
// dependency resolution of a package manager.
// To make it work properly we need also to setup some variables, configs and paths.
// This program will take care of that, and will make sure that the podman configuration
// does not overlap with the one eventually installed by a package manager, making
// this iteration of podman isolated from the rest.
//
//go:embed assets.tar.gz
var pack []byte

var version = "devel"

var (
	targetDir              = filepath.Join(os.Getenv("HOME"), ".local/share/podman-static")
	containersConf         = filepath.Join(targetDir, "/conf/containers/containers.conf")
	containersRegistryConf = filepath.Join(targetDir, "/conf/containers/registries.conf")
	containersStorageConf  = filepath.Join(targetDir, "/conf/containers/storage.conf")
	containersPolicyJSON   = filepath.Join(targetDir, "/conf/containers/policy.json")
)

func untar(reader io.Reader, dst string) error {
	err := os.MkdirAll(dst, 0o755)
	if err != nil {
		return err
	}

	extract := exec.Command("tar", "-xzf", "-", "-C", dst)
	extract.Stdin = reader

	return extract.Run()
}

// populate our container.conf file using the template given.
func setupContainerConf() error {
	containerConf := `[engine]
infra_image="k8s.gcr.io/pause:3.8"
events_logger="file"
exit_command_delay = 10
runtime = "crun"
stop_timeout = 5
conmon_path=[ "{{.Path}}/lib/podman/conmon" ]
helper_binaries_dir = [ "{{.Path}}/lib/podman" ]
static_dir = "{{.Path}}/share/podman/libpod"
volume_path = "{{.Path}}/share/podman/volume"
[engine.runtimes]
crun = [ "{{.Path}}/bin/crun" ]
runc = [ "{{.Path}}/bin/runc" ]
[network]
cni_plugin_dirs = [ "{{.Path}}/lib/cni" ]`

	tmpl, err := template.New("conf").Parse(containerConf)
	if err != nil {
		return err
	}

	// set the Path to our targetDir
	vars := make(map[string]interface{})
	vars["Path"] = targetDir

	// and save it
	file, err := os.Create(containersConf)
	if err != nil {
		return err
	}

	return tmpl.Execute(file, vars)
}

// setup storage.conf in order to point to our targetDIR and binaries correctly.
func setupStorageConf() error {
	storageConf, err := os.ReadFile(containersStorageConf)
	if err != nil {
		return err
	}

	// Replace /var with our directory, and point to our fuse-overlayfs binary
	content := bytes.ReplaceAll(storageConf, []byte("/var"), []byte(targetDir))
	content = bytes.ReplaceAll(content,
		[]byte("/usr/local/bin/fuse-overlayfs"),
		[]byte(filepath.Join(targetDir, "bin/fuse-overlayfs")))
	// and save the config file
	err = os.WriteFile(containersStorageConf, content, 0o600)
	if err != nil {
		return err
	}

	return nil
}

func setupConfs() error {
	// if we already ran the first setup, we don't overwrite the configs
	_, err := os.Stat(containersStorageConf)
	if err == nil {
		return nil
	}

	// if we didn't then copy the default configs from etc into conf and set them up
	err = exec.Command("cp", "-r", targetDir+"/etc", targetDir+"/conf").Run()
	if err != nil {
		return err
	}

	err = setupStorageConf()
	if err != nil {
		return err
	}

	err = setupContainerConf()
	if err != nil {
		return err
	}

	return nil
}

func main() {
	// if specific PODMAN_STATIC_TARGET_DIR is set, then use that instead
	if os.Getenv("PODMAN_STATIC_TARGET_DIR") != "" {
		targetDir = os.Getenv("PODMAN_STATIC_TARGET_DIR")
	}

	if len(os.Args) > 1 && os.Args[1] == "upgrade" {
		err := os.RemoveAll(filepath.Join(targetDir, "bin/podman"))
		if err != nil {
			panic(err)
		}

		cmd := exec.Command(os.Args[0], "info")
		cmd.Stderr = os.Stderr
		cmd.Stdout = os.Stdout
		cmd.Stdin = os.Stdin

		if err := cmd.Run(); err != nil {
			if exitError, ok := err.(*exec.ExitError); ok {
				os.Exit(exitError.ExitCode())
			}
		}

		os.Exit(0)
	}

	if len(os.Args) > 1 && os.Args[1] == "version" {
		println("Launcher:     " + version)
	}

	_, err := exec.LookPath("tar")
	if err != nil {
		println("missing dependency tar")
		os.Exit(127)
	}

	_, err = exec.LookPath("cp")
	if err != nil {
		println("missing dependency cp")
		os.Exit(127)
	}

	// we want our custom runtime directory for this podman environment, or
	// it might crash with an already running podman in /run/user/$UID/containers
	runtimeDir := filepath.Join("/var/tmp/podman-static/", strconv.Itoa(os.Getuid()))
	// set the --root and --runroot flags accordingly
	args := []string{
		"--root", filepath.Join(targetDir, "share/containers/storage"),
		"--runroot", filepath.Join(runtimeDir, "containers"),
	}

	// There isn't a config to inject the default signature policy in a place
	// other than /etc/containers/policy.jon
	//
	// So we will need to add the "--signature-policy" flag in the commands that
	// support it.
	if len(os.Args) > 1 && (os.Args[1] == "run" ||
		os.Args[1] == "build" ||
		os.Args[1] == "import" ||
		os.Args[1] == "load" ||
		os.Args[1] == "push" ||
		os.Args[1] == "save" ||
		os.Args[1] == "pull") {
		args = append(args, os.Args[1])
		args = append(args, "--signature-policy")
		args = append(args, containersPolicyJSON)
		// then we just forward all the flags to the child podman command
		args = append(args, os.Args[2:]...)
	} else if len(os.Args) > 2 && (os.Args[2] == "run" ||
		os.Args[2] == "build" ||
		os.Args[2] == "import" ||
		os.Args[2] == "load" ||
		os.Args[2] == "pull" ||
		os.Args[2] == "push" ||
		os.Args[2] == "save" ||
		os.Args[2] == "play") {
		args = append(args, os.Args[1])
		args = append(args, os.Args[2])
		args = append(args, "--signature-policy")
		args = append(args, containersPolicyJSON)
		// then we just forward all the flags to the child podman command
		args = append(args, os.Args[3:]...)
	} else {
		// else we just forward all the flags to the child podman command
		args = append(args, os.Args[1:]...)
	}

	// Setup our ENV to point to our custom files:
	//		https://docs.podman.io/en/latest/markdown/podman.1.html#environment-variables
	err = os.Setenv("CONTAINERS_CONF", containersConf)
	if err != nil {
		panic(err)
	}

	err = os.Setenv("CONTAINERS_REGISTRIES_CONF", containersRegistryConf)
	if err != nil {
		panic(err)
	}

	err = os.Setenv("CONTAINERS_STORAGE_CONF", containersStorageConf)
	if err != nil {
		panic(err)
	}

	// give precedence to our binaries in this context
	err = os.Setenv("PATH", targetDir+"/bin:"+os.Getenv("PATH"))
	if err != nil {
		panic(err)
	}

	// create our unpack dir
	err = os.MkdirAll(targetDir, 0o755)
	if err != nil {
		panic(err)
	}

	// if we don't have podman in our target dir, then unpack it
	_, err = os.Stat(filepath.Join(targetDir, "bin/podman"))
	if err != nil {
		err = untar(bytes.NewReader(pack), targetDir)
		if err != nil {
			panic(err)
		}

		// setup our custom configs
		err = setupConfs()
		if err != nil {
			panic(err)
		}
	}

	// we need to make sure the runtime dir is present, or podman will complain
	err = os.MkdirAll(runtimeDir, 0o755)
	if err != nil {
		panic(err)
	}

	// this also helps to separate the crun instances
	err = os.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	if err != nil {
		panic(err)
	}

	command := filepath.Join(targetDir, "bin/podman")
	args = append([]string{command}, args...)

	// execve podman
	err = syscall.Exec(command, args, os.Environ())
	if err != nil {
		panic(err)
	}
}
