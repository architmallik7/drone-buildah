package docker

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

const buildahExe = "buildah"

type (
	// Login defines Docker login parameters.
	Login struct {
		Registry string // Docker registry address
		Username string // Docker registry username
		Password string // Docker registry password
		Email    string // Docker registry email
		Config   string // Docker Auth Config
	}

	// Build defines Docker build parameters.
	Build struct {
		Remote      string   // Git remote URL
		Name        string   // Docker build using default named tag
		Dockerfile  string   // Docker build Dockerfile
		Context     string   // Docker build context
		Tags        []string // Docker build tags
		Args        []string // Docker build args
		ArgsEnv     []string // Docker build args from env
		Target      string   // Docker build target
		Squash      bool     // Docker build squash
		Pull        bool     // Docker build pull
		CacheFrom   []string // Docker build cache-from
		Compress    bool     // Docker build compress
		Repo        string   // Docker build repository
		LabelSchema []string // label-schema Label map
		AutoLabel   bool     // auto-label bool
		Labels      []string // Label map
		Link        string   // Git repo link
		NoCache     bool     // Docker build no-cache
		AddHost     []string // Docker build add-host
		Quiet       bool     // Docker build quiet
		S3CacheDir  string
		S3Bucket    string
		S3Endpoint  string
		S3Region    string
		S3Key       string
		S3Secret    string
		S3UseSSL    bool
		Layers      bool
	}

	// Plugin defines the Docker plugin parameters.
	Plugin struct {
		Login    Login // Docker login configuration
		Build    Build // Docker build configuration
		SkipPush bool  // Docker push is skipped if true
		Cleanup  bool  // Docker purge is enabled
	}
)

// Exec executes the plugin step
func (p Plugin) Exec() error {
	// Set up custom storage configuration for rootless mode
	user, err := user.Current()
	if err != nil {
		return fmt.Errorf("error getting current user: %s", err)
	}

	storageConfDir := filepath.Join(user.HomeDir, ".config", "containers")
	if err := os.MkdirAll(storageConfDir, 0700); err != nil {
		return fmt.Errorf("error creating storage config directory: %s", err)
	}

	storageConfPath := filepath.Join(storageConfDir, "storage.conf")
	storageConf := `[storage]
	driver = "vfs"
	runroot = "/tmp/buildah-run-$(id -u)"
	graphroot = "/tmp/buildah-graph-$(id -u)"`

	if err := ioutil.WriteFile(storageConfPath, []byte(storageConf), 0600); err != nil {
		return fmt.Errorf("error writing storage.conf: %s", err)
	}

	// Set environment variables for rootless mode
	os.Setenv("STORAGE_DRIVER", "vfs")
	os.Setenv("BUILDAH_ISOLATION", "rootless")
	os.Setenv("CONTAINERS_STORAGE_CONF", storageConfPath)

	// Create Auth Config File
	if p.Login.Config != "" {
		authPath := filepath.Join(storageConfDir, "auth.json")
		if err := ioutil.WriteFile(authPath, []byte(p.Login.Config), 0600); err != nil {
			return fmt.Errorf("error writing auth.json: %s", err)
		}
		fmt.Printf("Config written to %s\n", authPath)
	}

	// login to the Docker registry
	if p.Login.Password != "" {
		cmd := commandLogin(p.Login)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		if err != nil {
			return fmt.Errorf("error authenticating: %s", err)
		}
	}

	switch {
	case p.Login.Password != "":
		fmt.Println("Detected registry credentials")
	case p.Login.Config != "":
		fmt.Println("Detected registry credentials file")
	default:
		fmt.Println("Registry credentials or Docker config not provided. Guest mode enabled.")
	}

	// add proxy build args
	addProxyBuildArgs(&p.Build)

	var cmds []*exec.Cmd
	cmds = append(cmds, commandVersion())
	cmds = append(cmds, commandInfo())

	// pre-pull cache images
	for _, img := range p.Build.CacheFrom {
		cmds = append(cmds, commandPull(img))
	}

	cmds = append(cmds, commandBuild(p.Build))

	for _, tag := range p.Build.Tags {
		cmds = append(cmds, commandTag(p.Build, tag))

		if !p.SkipPush {
			cmds = append(cmds, commandPush(p.Build, tag))
		}
	}

	if p.Cleanup {
		cmds = append(cmds, commandRmi(p.Build.Name))
	}

	// execute all commands in batch mode.
	for _, cmd := range cmds {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		trace(cmd)

		err := cmd.Run()
		if err != nil {
			if isCommandPull(cmd.Args) {
				fmt.Printf("Could not pull cache-from image %s. Ignoring...\n", cmd.Args[2])
			} else if isCommandPrune(cmd.Args) {
				fmt.Printf("Could not prune system containers. Ignoring...\n")
			} else if isCommandRmi(cmd.Args) {
				fmt.Printf("Could not remove image %s. Ignoring...\n", cmd.Args[2])
			} else {
				return err
			}
		}
	}

	return nil
}

func commandLogin(login Login) *exec.Cmd {
	return exec.Command(
		buildahExe, "login",
		"-u", login.Username,
		"-p", login.Password,
		login.Registry,
	)
}

func isCommandPull(args []string) bool {
	return len(args) > 2 && args[1] == "pull"
}

func commandPull(repo string) *exec.Cmd {
	return exec.Command(buildahExe, "pull", "--storage-driver", "vfs", repo)
}

func commandVersion() *exec.Cmd {
	return exec.Command(buildahExe, "version")
}

func commandInfo() *exec.Cmd {
	return exec.Command(buildahExe, "info")
}

func commandBuild(build Build) *exec.Cmd {
	args := []string{
		"bud",
		"--format", "docker",
		"-f", build.Dockerfile,
	}

	if build.Squash {
		args = append(args, "--squash")
	}
	if build.Compress {
		args = append(args, "--compress")
	}
	if build.Pull {
		args = append(args, "--pull=true")
	}
	if build.NoCache {
		args = append(args, "--no-cache")
	}
	for _, arg := range build.CacheFrom {
		args = append(args, "--cache-from", arg)
	}
	for _, arg := range build.ArgsEnv {
		addProxyValue(&build, arg)
	}
	for _, arg := range build.Args {
		args = append(args, "--build-arg", arg)
	}
	for _, host := range build.AddHost {
		args = append(args, "--add-host", host)
	}
	if build.Target != "" {
		args = append(args, "--target", build.Target)
	}
	if build.Quiet {
		args = append(args, "--quiet")
	}
	if build.Layers {
		args = append(args, "--layers=true")
		if build.S3CacheDir != "" {
			args = append(args, "--s3-local-cache-dir", build.S3CacheDir)
			if build.S3Bucket != "" {
				args = append(args, "--s3-bucket", build.S3Bucket)
			}
			if build.S3Endpoint != "" {
				args = append(args, "--s3-endpoint", build.S3Endpoint)
			}
			if build.S3Region != "" {
				args = append(args, "--s3-region", build.S3Region)
			}
			if build.S3Key != "" {
				args = append(args, "--s3-key", build.S3Key)
			}
			if build.S3Secret != "" {
				args = append(args, "--s3-secret", build.S3Secret)
			}
			if build.S3UseSSL {
				args = append(args, "--s3-use-ssl=true")
			}
		}
	}

	if build.AutoLabel {
		labelSchema := []string{
			fmt.Sprintf("created=%s", time.Now().Format(time.RFC3339)),
			fmt.Sprintf("revision=%s", build.Name),
			fmt.Sprintf("source=%s", build.Remote),
			fmt.Sprintf("url=%s", build.Link),
		}
		labelPrefix := "org.opencontainers.image"

		if len(build.LabelSchema) > 0 {
			labelSchema = append(labelSchema, build.LabelSchema...)
		}

		for _, label := range labelSchema {
			args = append(args, "--label", fmt.Sprintf("%s.%s", labelPrefix, label))
		}
	}

	if len(build.Labels) > 0 {
		for _, label := range build.Labels {
			args = append(args, "--label", label)
		}
	}

	args = append(args, "-t", build.Name)
	args = append(args, build.Context)
	return exec.Command(buildahExe, args...)
}

func addProxyBuildArgs(build *Build) {
	addProxyValue(build, "http_proxy")
	addProxyValue(build, "https_proxy")
	addProxyValue(build, "no_proxy")
}

func addProxyValue(build *Build, key string) {
	value := getProxyValue(key)

	if len(value) > 0 && !hasProxyBuildArg(build, key) {
		build.Args = append(build.Args, fmt.Sprintf("%s=%s", key, value))
		build.Args = append(build.Args, fmt.Sprintf("%s=%s", strings.ToUpper(key), value))
	}
}

func getProxyValue(key string) string {
	value := os.Getenv(key)

	if len(value) > 0 {
		return value
	}

	return os.Getenv(strings.ToUpper(key))
}

func hasProxyBuildArg(build *Build, key string) bool {
	keyUpper := strings.ToUpper(key)

	for _, s := range build.Args {
		if strings.HasPrefix(s, key) || strings.HasPrefix(s, keyUpper) {
			return true
		}
	}

	return false
}

func commandTag(build Build, tag string) *exec.Cmd {
	var (
		source = build.Name
		target = fmt.Sprintf("%s:%s", build.Repo, tag)
	)
	return exec.Command(buildahExe, "tag", source, target)
}

func commandPush(build Build, tag string) *exec.Cmd {
	target := fmt.Sprintf("%s:%s", build.Repo, tag)
	return exec.Command(buildahExe, "push", target)
}

func commandRmi(tag string) *exec.Cmd {
	return exec.Command(buildahExe, "rmi", tag)
}

func isCommandPrune(args []string) bool {
	return len(args) > 3 && args[2] == "prune"
}

func isCommandRmi(args []string) bool {
	return len(args) > 2 && args[1] == "rmi"
}

func trace(cmd *exec.Cmd) {
	fmt.Fprintf(os.Stdout, "+ %s\n", strings.Join(cmd.Args, " "))
}
