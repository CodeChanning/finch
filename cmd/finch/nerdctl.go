// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/exp/slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/sirupsen/logrus"
	"github.com/spf13/afero"
	"github.com/spf13/cobra"

	"github.com/runfinch/finch/pkg/command"
	"github.com/runfinch/finch/pkg/config"
	"github.com/runfinch/finch/pkg/flog"
	"github.com/runfinch/finch/pkg/lima"
	"github.com/runfinch/finch/pkg/system"

	"k8s.io/apimachinery/pkg/util/sets"
)

const nerdctlCmdName = "nerdctl"

// NerdctlCommandSystemDeps contains the system dependencies for newNerdctlCommand.
//
//go:generate mockgen -copyright_file=../../copyright_header -destination=../../pkg/mocks/nerdctl_cmd_system_deps.go -package=mocks -mock_names NerdctlCommandSystemDeps=NerdctlCommandSystemDeps -source=nerdctl.go NerdctlCommandSystemDeps
type NerdctlCommandSystemDeps interface {
	system.EnvChecker
	system.WorkingDirectory
	system.FilePathJoiner
	system.AbsFilePath
	system.FilePathToSlash
}

type nerdctlCommandCreator struct {
	lcc        command.LimaCmdCreator
	ecc        command.Creator
	systemDeps NerdctlCommandSystemDeps
	logger     flog.Logger
	fs         afero.Fs
	fc         *config.Finch
}

type (
	argHandler     func(systemDeps NerdctlCommandSystemDeps, args []string, index int) error
	commandHandler func(systemDeps NerdctlCommandSystemDeps, args []string) error
)

func newNerdctlCommandCreator(
	lcc command.LimaCmdCreator,
	ecc command.Creator,
	systemDeps NerdctlCommandSystemDeps,
	logger flog.Logger,
	fs afero.Fs,
	fc *config.Finch,
) *nerdctlCommandCreator {
	return &nerdctlCommandCreator{lcc: lcc, ecc: ecc, systemDeps: systemDeps, logger: logger, fs: fs, fc: fc}
}

func (ncc *nerdctlCommandCreator) create(cmdName string, cmdDesc string) *cobra.Command {
	command := &cobra.Command{
		Use:   cmdName,
		Short: cmdDesc,
		// If we don't specify it, and the user issues `finch run -d alpine`,
		// the args passed to nerdctlCommand.run will be empty because
		// cobra will try to parse `-d alpine` as if alpine is the value of the `-d` flag.
		DisableFlagParsing: true,
		RunE:               newNerdctlCommand(ncc.lcc, ncc.ecc, ncc.systemDeps, ncc.logger, ncc.fs, ncc.fc).runAdapter,
	}

	return command
}

type nerdctlCommand struct {
	lcc        command.LimaCmdCreator
	ecc        command.Creator
	systemDeps NerdctlCommandSystemDeps
	logger     flog.Logger
	fs         afero.Fs
	fc         *config.Finch
}

func newNerdctlCommand(
	lcc command.LimaCmdCreator,
	ecc command.Creator,
	systemDeps NerdctlCommandSystemDeps,
	logger flog.Logger,
	fs afero.Fs,
	fc *config.Finch,
) *nerdctlCommand {
	return &nerdctlCommand{lcc: lcc, ecc: ecc, systemDeps: systemDeps, logger: logger, fs: fs, fc: fc}
}

func (nc *nerdctlCommand) runAdapter(cmd *cobra.Command, args []string) error {
	return nc.run(cmd.Name(), args)
}

func (nc *nerdctlCommand) run(cmdName string, args []string) error {
	err := nc.assertVMIsRunning(nc.lcc, nc.logger)
	if err != nil {
		return err
	}
	var (
		nerdctlArgs, envs, fileEnvs       []string
		skip, hasCmdHander, hasArgHandler bool
		cmdHandler                        commandHandler
		aMap                              map[string]argHandler
		envArgPos                         int
		isDebug                           int
		// buildx                            bool
		// version                           bool
	)

	alias, hasAlias := aliasMap[cmdName]
	if hasAlias {
		cmdName = alias
		cmdHandler, hasCmdHander = commandHandlerMap[alias]
		aMap, hasArgHandler = argHandlerMap[alias]
	} else {
		// Check if the command has a handler
		cmdHandler, hasCmdHander = commandHandlerMap[cmdName]
		aMap, hasArgHandler = argHandlerMap[cmdName]

		if !hasCmdHander && !hasArgHandler && len(args) > 0 {
			// for commands like image build, container run
			key := fmt.Sprintf("%s %s", cmdName, args[0])
			cmdHandler, hasCmdHander = commandHandlerMap[key]
			aMap, hasArgHandler = argHandlerMap[key]
		}
	}

	// First check if the command has command handler
	if hasCmdHander {
		err := cmdHandler(nc.systemDeps, args)
		if err != nil {
			return err
		}
	}
	// buildx = false
	// version = false
	// notShiftEnv := false
	// envArgPos is used to preserve the position of first environment parameter
	envArgPos = -1
	// if a debug flag is passed before env arg pos we reduce the env arg pos by 1 to account for skipping debug flag
	isDebug = 0
	for i, arg := range args {
		logrus.Warnf("looking at arg %s at index %d", arg, i)

		// Check if command requires arg handling
		if hasArgHandler {
			// Check if argument for the command needs handling, sometimes it can be --file=<filename>
			b, _, _ := strings.Cut(arg, "=")
			h, ok := aMap[b]
			if ok {
				err = h(nc.systemDeps, args, i)
				if err != nil {
					return err
				}
				// This is required when the positional argument at i is mutated by argHandler, eg -v=C:\Users:/tmp:ro
				arg = args[i]
			}
		}

		// prefix := "/home/vscode/.vscode-server/bin/"
		// suffix := "/node"
		// // Check if the string has the correct prefix and suffix
		// if strings.HasPrefix(arg, prefix) && strings.HasSuffix(arg, suffix) {
		// 	notShiftEnv = true
		// }

		handleCache(nc.fc, &arg, &cmdName)
		// // Hack to handle consistency params during mounts. This is assuming no other commands or env variable will have the word consistency.
		// // Final implementation needs to account for this is only applied on darwin OS and for mount with a dockercompat flag enabled in finch.yaml file
		// // This probably requires a small design doc for the dockercompat flag which can be passed onto nerdctl if enabled.
		// if strings.Contains(arg, "consistency") {
		// 	arg = strings.Replace(arg, ",consistency=cache", "", 1)
		// 	arg = strings.Replace(arg, ",consistency=delegated", "", 1)
		// 	arg = strings.Replace(arg, ",consistency=consistent", "", 1)
		// }

		/**

		implementation notes:
		//finch.yaml: mode=dockercompat

		//all of this is only done with dockercompat
		//print warnings when converting

		//cache:
		//print warning when taking cout cahce options
		//remove cahce options if GOOS=darwin & with mount & dockercompat on (mac is called darwin)

		//buildx:
		//finch buildx -> nerdctl build
		//finch buildx build -> nerdctl build

		////can have --load flag
		////--load --> "--output=type=docker"

		////version:
		////print buildkit version
		////nerdctl build version doesnt return buildkit version
		////call finch command that does this

		////dont worrry about notShiftEnv
		////we dont handle env variables correctly
		////ticket for that in finch **/

		// parsing environment values from the command line may pre-fetch and
		// consume the next argument; this loop variable will skip these pre-consumed
		// entries from the command line
		if skip {
			skip = false
			continue
		}
		switch {
		case arg == "--debug":
			// explicitly setting log level to avoid `--debug` flag being interpreted as nerdctl command
			if envArgPos == -1 {
				isDebug = 1
			}
			nc.logger.SetLevel(flog.Debug)

		// This is a docker specific command which alias for --output=type=docker. This should only applied for build args.
		// On a long term this run command potentially needs to be refactored, currently it is too hacky the way it handles the args.
		case arg == "--load":
			// arg_mod := "--output=type=docker"
			// nerdctlArgs = append(nerdctlArgs, arg_mod)
			nc.logger.Info("found --load converting to --output flag")
			handleLoad(nc.fc, &nerdctlArgs, i)

		case argIsEnv(arg):
			// if notShiftEnv {
			// 	nerdctlArgs = append(nerdctlArgs, arg)
			// 	continue
			// }
			if envArgPos == -1 {
				envArgPos = i - isDebug
			}
			shouldSkip, addEnv := handleEnv(nc.systemDeps, arg, args[i+1])
			skip = shouldSkip
			if addEnv != "" {
				envs = append(envs, addEnv)
			}
		case strings.HasPrefix(arg, "--env-file"):
			if envArgPos == -1 {
				envArgPos = i - isDebug
			}

			shouldSkip, addEnvs, err := handleEnvFile(nc.fs, nc.systemDeps, arg, args[i+1])
			if err != nil {
				return err
			}
			skip = shouldSkip
			fileEnvs = append(fileEnvs, addEnvs...)
		case strings.HasPrefix(arg, "--add-host"):
			switch arg {
			case "--add-host":
				args[i+1], err = resolveIP(args[i+1], nc.logger, nc.ecc)
				if err != nil {
					return err
				}
			default:
				resolvedIP, err := resolveIP(arg[11:], nc.logger, nc.ecc)
				if err != nil {
					return err
				}
				arg = fmt.Sprintf("%s%s", arg[0:11], resolvedIP)
			}
			nerdctlArgs = append(nerdctlArgs, arg)
			if err != nil {
				return err
			}
		default:
			nerdctlArgs = append(nerdctlArgs, arg)
		}
	}

	// to handle environment variables properly, we add all entries found via
	// env-file includes to the map first and then all command line environment
	// flags, making sure that command line overrides environment file options,
	// and that later command line flags override earlier ones
	envVars := make(map[string]string)

	for _, e := range fileEnvs {
		evar, eval, _ := strings.Cut(e, "=")
		envVars[evar] = eval
	}
	for _, e := range envs {
		evar, eval, _ := strings.Cut(e, "=")
		envVars[evar] = eval
	}

	passedEnvs := []string{
		"COSIGN_PASSWORD", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY",
		"AWS_SESSION_TOKEN", "COMPOSE_FILE",
	}

	var passedEnvArgs []string
	for _, e := range passedEnvs {
		v, b := nc.systemDeps.LookupEnv(e)
		if !b {
			continue
		}
		if runtime.GOOS == "windows" && e == "COMPOSE_FILE" {
			wslPath, err := convertToWSLPath(nc.systemDeps, v)
			if err != nil {
				return err
			}
			v = wslPath
		}
		passedEnvArgs = append(passedEnvArgs, fmt.Sprintf("%s=%s", e, v))
	}

	var additionalEnv []string
	switch cmdName {
	case "image":
		if slices.Contains(args, "build") || slices.Contains(args, "pull") || slices.Contains(args, "push") {
			ensureRemoteCredentials(nc.fc, nc.ecc, &additionalEnv, nc.logger)
		}
	case "container":
		if slices.Contains(args, "run") {
			ensureRemoteCredentials(nc.fc, nc.ecc, &additionalEnv, nc.logger)
		}
	case "build", "pull", "push", "run":
		ensureRemoteCredentials(nc.fc, nc.ecc, &additionalEnv, nc.logger)
	}

	// Add -E to sudo command in order to preserve existing environment variables, more info:
	// https://stackoverflow.com/questions/8633461/how-to-keep-environment-variables-when-using-sudo/8633575#8633575
	limaArgs := append(nc.GetLimaArgs(), append(additionalEnv, passedEnvArgs...)...)

	limaArgs = append(limaArgs, append([]string{nerdctlCmdName}, strings.Fields(cmdName)...)...)

	var envArgs []string

	// // This is to fix env variables that are part of the exec commands. This is a hack, cr would need to be flushed out.
	// for key, val := range envVars {
	// 	strEncode := fmt.Sprintf("%s=%s", key, val)
	// 	envArgs = append(envArgs, "-e", strEncode)
	// }
	if envArgPos > -1 {
		nerdctlArgs = append(nerdctlArgs[:envArgPos], append(envArgs, nerdctlArgs[envArgPos:]...)...)
	}

	// Add -E to sudo command in order to preserve existing environment variables, more info:
	// https://stackoverflow.com/questions/8633461/how-to-keep-environment-variables-when-using-sudo/8633575#8633575
	limaArgs = append(limaArgs, nerdctlArgs...)

	if nc.shouldReplaceForHelp(cmdName, args) {
		return nc.lcc.RunWithReplacingStdout([]command.Replacement{{Source: "nerdctl", Target: "finch"}}, limaArgs...)
	}

	// // Handle buildx version and build commands.
	// // We should throw the buildkit version instead, this is just fooling dev containers atm.
	// var limaArgs2 []string
	// for _, arg := range limaArgs {
	// 	if arg == "buildx" {
	// 		buildx = true
	// 	} else if arg == "version" && buildx {
	// 		version = true
	// 	} else {
	// 		limaArgs2 = append(limaArgs2, arg)
	// 	}
	// }

	// if version {
	// 	fmt.Printf("github.com/docker/buildx v0.12.1-desktop.4 6996841df2f61988c2794d84d33205368f96c317")
	// 	return nil
	// }

	// Handle buildx version and build commands.
	skipCmd, limaArgs := handleBuildx(nc.fc, &limaArgs)
	if skipCmd {
		return nil
	}

	nc.logger.Info("Running nerdctl command args ", limaArgs, "  end")

	return nc.lcc.Create(limaArgs...).Run()
}

func (nc *nerdctlCommand) assertVMIsRunning(creator command.LimaCmdCreator, logger flog.Logger) error {
	// Extra call to check VM before running nerdctl commands. These are the reasons of not doing message replacing
	// 1. for the non-help commands, replacing stdout may cause "stdin is not a terminal" error for the commands that need input.
	// E.g. finch login.
	// 2. an extra call could give us more control about the error messages. Message replacing may fail if upstream
	// changes the format of source string, which leads to extra CI validation work.
	status, err := lima.GetVMStatus(creator, logger, limaInstanceName)
	if err != nil {
		return err
	}
	switch status {
	case lima.Nonexistent:
		return fmt.Errorf("instance %q does not exist, run `finch %s init` to create a new instance",
			limaInstanceName, virtualMachineRootCmd)
	case lima.Stopped:
		return fmt.Errorf("instance %q is stopped, run `finch %s start` to start the instance",
			limaInstanceName, virtualMachineRootCmd)
	default:
		return nil
	}
}

// shouldReplaceForHelp returns true if we should replace "nerdctl" with "finch" for the output of the given command.
func (nc *nerdctlCommand) shouldReplaceForHelp(cmdName string, args []string) bool {
	// The implicit help commands mean that if users input "finch" without any args, it will return the help of it.
	// Not all the commands are implicit help commands
	implicitHelpCmdSet := sets.NewString("system", "builder", "compose", "container", "image", "network", "volume")

	if len(args) == 0 {
		if implicitHelpCmdSet.Has(cmdName) {
			return true
		}
	}

	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			return true
		}
	}

	return false
}

func argIsEnv(arg string) bool {
	if strings.HasPrefix(arg, "-e") || (strings.HasPrefix(arg, "--env") && !strings.HasPrefix(arg, "--env-file")) {
		return true
	}
	return false
}

func handleCache(fc *config.Finch, arg *string, cmdName *string) {
	// Hack to handle consistency params during mounts. This is assuming no other commands or env variable will have the word consistency.
	// Final implementation needs to account for this is only applied on darwin OS and for mount with a dockercompat flag enabled in finch.yaml file
	// This probably requires a small design doc for the dockercompat flag which can be passed onto nerdctl if enabled.
	if *fc.Mode == "dockercompat" && runtime.GOOS == "darwin" && *cmdName == "mount" {
		if strings.Contains(*arg, "consistency") {
			*arg = strings.Replace(*arg, ",consistency=cache", "", 1)
			*arg = strings.Replace(*arg, ",consistency=delegated", "", 1)
			*arg = strings.Replace(*arg, ",consistency=consistent", "", 1)
		}
	}
}

func handleLoad(fc *config.Finch, args *[]string, idx int) {
	if *fc.Mode == "dockercompat" {
		logrus.Warn("appending --output-type!!")
		logrus.Warn("args before appending", args)
		*args = append((*args), "--output=type=docker")
		logrus.Warn("args after appending", args)
	}
}

func handleBuildx(fc *config.Finch, limaArgs *[]string) (bool, []string) {
	logrus.Warn("handling buildx")

	buildx := false
	skipCmd := true
	var newLimaArgs []string
	buildxSubcommands := []string{"bake", "create", "debug", "du", "imagetools", "inspect", "ls", "prune", "rm", "stop", "use", "version"}

	if *fc.Mode != "dockercompat" {
		return !skipCmd, *limaArgs
	}

	for idx, arg := range *limaArgs {
		logrus.Warnf("looking at arg %s at index %d", arg, idx)

		if arg == "buildx" {
			buildx = true
			newLimaArgs = append((*limaArgs)[:(idx)], "build")
			logrus.Warn("buildx is not supported. using standard buildkit instead...")
		} else if buildx {

			//checking for buildx subcommands
			//for buildx build we call nerdctl build,
			//for any other case we print a warning message and don't run a command
			buildxWarnMsg := "buildx %s command is not supported."

			if arg == "build" {
				continue
			} else if slices.Contains(buildxSubcommands, arg) {
				logrus.Warnf(buildxWarnMsg, arg)
				return skipCmd, nil
			} else {
				newLimaArgs = append((*limaArgs)[:(idx)], arg)
			}

		} else {
			newLimaArgs = append((*limaArgs)[:(idx)], arg)
		}
	}

	return !skipCmd, newLimaArgs
}

func handleEnv(systemDeps NerdctlCommandSystemDeps, arg, arg2 string) (bool, string) {
	var (
		envVar string
		skip   bool
	)

	switch arg {
	case "-e", "--env":
		skip = true
		envVar = arg2
	default:
		// flag and value are in the same string
		if strings.HasPrefix(arg, "-e") {
			envVar = arg[2:]
		} else {
			// only other case is "--env="; skip that prefix
			envVar = arg[6:]
		}
	}

	if strings.Contains(envVar, "=") {
		return skip, envVar
	}
	// if no value was provided we need to check the OS environment
	// for a value and only set if it exists in the current env
	if val, ok := systemDeps.LookupEnv(envVar); ok {
		return skip, fmt.Sprintf("%s=%s", envVar, val)
	}
	// no value found; do not set the variable in the env
	return skip, ""
}

func handleEnvFile(fs afero.Fs, systemDeps NerdctlCommandSystemDeps, arg, arg2 string) (bool, []string, error) {
	var (
		filename string
		skip     bool
	)

	switch arg {
	case "--env-file":
		skip = true
		filename = arg2
	default:
		filename = arg[11:]
	}

	file, err := fs.Open(filepath.Clean(filename))
	if err != nil {
		return false, []string{}, err
	}
	defer file.Close() //nolint:errcheck // We did not write to the file, and the file will be closed when the CLI process exits anyway.

	scanner := bufio.NewScanner(file)

	var envs []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if len(line) == 0 {
			continue
		}
		switch {
		case strings.HasPrefix(line, "#"):
			// ignore comments
			continue
		case !strings.Contains(line, "="):
			// only has the variable name; need to lookup value
			if val, ok := systemDeps.LookupEnv(line); ok {
				envs = append(envs, fmt.Sprintf("%s=%s", line, val))
			}
		default:
			// contains a name and value
			envs = append(envs, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return skip, []string{}, err
	}
	return skip, envs, nil
}

// ensureRemoteCredentials is called before any actions that may require remote resources, in order
// to ensure that fresh credentials are available inside the VM.
// For more details on how `aws configure export-credentials` works, checks the docs.
//
// [the docs]: https://awscli.amazonaws.com/v2/documentation/api/latest/reference/configure/export-credentials.html
func ensureRemoteCredentials(
	fc *config.Finch,
	ecc command.Creator,
	outEnv *[]string,
	logger flog.Logger,
) {
	if slices.Contains(fc.CredsHelpers, "ecr-login") {
		out, err := ecc.Create(
			"aws",
			"configure",
			"export-credentials",
			"--format",
			"process",
		).CombinedOutput()
		if err != nil {
			logger.Debugln("failed to run `aws configure` command")
			return
		}

		var exportCredsOut aws.Credentials
		err = json.Unmarshal(out, &exportCredsOut)
		if err != nil {
			logger.Debugln("`aws configure export-credentials` output is unexpected, is command available? " +
				"This may result in a broken ecr-credential helper experience.")
			return
		}

		*outEnv = append(*outEnv, fmt.Sprintf("AWS_ACCESS_KEY_ID=%s", exportCredsOut.AccessKeyID))
		*outEnv = append(*outEnv, fmt.Sprintf("AWS_SECRET_ACCESS_KEY=%s", exportCredsOut.SecretAccessKey))
		*outEnv = append(*outEnv, fmt.Sprintf("AWS_SESSION_TOKEN=%s", exportCredsOut.SessionToken))
	}
}

var nerdctlCmds = map[string]string{
	"build":     "Build an image from Dockerfile",
	"builder":   "Manage builds",
	"commit":    "Create a new image from a container's changes",
	"compose":   "Compose",
	"container": "Manage containers",
	"create":    "Create a new container",
	"cp":        "Copy files/folders between a running container and the local filesystem",
	"events":    "Get real time events from the server",
	"exec":      "Run a command in a running container",
	"history":   "Show the history of an image",
	"image":     "Manage images",
	"images":    "List images",
	"info":      "Display system-wide information",
	"inspect":   "Return low-level information on Docker objects",
	"kill":      "Kill one or more running containers",
	"load":      "Load an image from a tar archive or STDIN",
	"login":     "Log in to a container registry",
	"logout":    "Log out from a container registry",
	"logs":      "Fetch the logs of a container",
	"network":   "Manage networks",
	"pause":     "Pause all processes within one or more containers",
	"port":      "List port mappings or a specific mapping for the container",
	"ps":        "List containers",
	"pull":      "Pull an image from a registry",
	"push":      "Push an image or a repository to a registry",
	"restart":   "Restart one or more containers",
	"rm":        "Remove one or more containers",
	"rmi":       "Remove one or more images",
	"run":       "Run a command in a new container",
	"save":      "Save one or more images to a tar archive (streamed to STDOUT by default)",
	"start":     "Start one or more stopped containers",
	"stats":     "Display a live stream of container(s) resource usage statistics",
	"stop":      "Stop one or more running containers",
	"system":    "Manage containerd",
	"tag":       "Create a tag TARGET_IMAGE that refers to SOURCE_IMAGE",
	"top":       "Display the running processes of a container",
	"unpause":   "Unpause all processes within one or more containers",
	"update":    "Update configuration of one or more containers",
	"volume":    "Manage volumes",
	"wait":      "Block until one or more containers stop, then print their exit codes",
}

var dockerCompatCmds = map[string]string{
	"buildx": "build version",
}
