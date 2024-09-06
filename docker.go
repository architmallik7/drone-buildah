package docker

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
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
		Login   Login // Docker login configuration
		Build   Build // Docker build configuration
		Dryrun  bool  // Docker push is skipped
		Cleanup bool  // Docker purge is enabled
	}
)

// Exec executes the plugin step
func (p Plugin) Exec() error {
	// Set the STORAGE_DRIVER environment variable
	os.Setenv("STORAGE_DRIVER", "vfs")

	// Create temporary directories
	tmpDir := "/tmp/buildah-plugin"
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return fmt.Errorf("error creating temporary directory: %w", err)
	}

	// Create a custom storage.conf file
	storageConf := fmt.Sprintf(`
[storage]
driver = "vfs"
runroot = "%s/runroot"
graphroot = "%s/graphroot"
`, tmpDir, tmpDir)

	storageConfPath := filepath.Join(tmpDir, "storage.conf")
	if err := ioutil.WriteFile(storageConfPath, []byte(storageConf), 0644); err != nil {
		return fmt.Errorf("error writing storage.conf: %w", err)
	}
	os.Setenv("CONTAINERS_STORAGE_CONF", storageConfPath)

	// Create Auth Config File
	if p.Login.Config != "" {
		if err := createAuthConfig(p.Login.Config, tmpDir); err != nil {
			return err
		}
	}

	// Login to the Docker registry
	if p.Login.Password != "" {
		if err := dockerLogin(p.Login); err != nil {
			return err
		}
	}

	// Add proxy build args
	addProxyBuildArgs(&p.Build)

	var cmds []*exec.Cmd
	cmds = append(cmds, commandVersion()) // buildah version
	cmds = append(cmds, commandInfo())    // buildah info

	// Pre-pull cache images
	for _, img := range p.Build.CacheFrom {
		cmds = append(cmds, commandPull(img))
	}

	cmds = append(cmds, commandBuild(p.Build)) // buildah build

	for _, tag := range p.Build.Tags {
		cmds = append(cmds, commandTag(p.Build, tag)) // buildah tag

		if !p.Dryrun {
			cmds = append(cmds, commandPush(p.Build, tag)) // buildah push
		}
	}

	if p.Cleanup {
		cmds = append(cmds, commandRmi(p.Build.Name)) // buildah rmi
	}

	// Execute all commands in batch mode.
	for _, cmd := range cmds {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		trace(cmd)

		err := cmd.Run()
		if err != nil {
			return fmt.Errorf("command failed: %s\nerror: %w", strings.Join(cmd.Args, " "), err)
		}
	}

	return nil
}

func createAuthConfig(config, tmpDir string) error {
	authDir := filepath.Join(tmpDir, "auth")
	if err := os.MkdirAll(authDir, 0700); err != nil {
		return fmt.Errorf("error creating auth directory: %w", err)
	}

	authPath := filepath.Join(authDir, "auth.json")
	if err := ioutil.WriteFile(authPath, []byte(config), 0600); err != nil {
		return fmt.Errorf("error writing auth.json: %w", err)
	}

	os.Setenv("REGISTRY_AUTH_FILE", authPath)
	fmt.Printf("Config written to %s\n", authPath)
	return nil
}

func dockerLogin(login Login) error {
	cmd := commandLogin(login)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("error authenticating: %w", err)
	}
	return nil
}

func commandLogin(login Login) *exec.Cmd {
	args := []string{"--storage-driver", "vfs", "login", "-u", login.Username, "-p", login.Password}
	if login.Email != "" {
		args = append(args, "-e", login.Email)
	}
	args = append(args, login.Registry)
	return exec.Command(buildahExe, args...)
}

func commandPull(repo string) *exec.Cmd {
	return exec.Command(buildahExe, "--storage-driver", "vfs", "pull", repo)
}

func commandVersion() *exec.Cmd {
	return exec.Command(buildahExe, "--storage-driver", "vfs", "version")
}

func commandInfo() *exec.Cmd {
	return exec.Command(buildahExe, "--storage-driver", "vfs", "info")
}

func commandBuild(build Build) *exec.Cmd {
	args := []string{
		"--storage-driver", "vfs",
		"bud",
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
	return exec.Command(buildahExe, "--storage-driver", "vfs", "tag", source, target)
}

func commandPush(build Build, tag string) *exec.Cmd {
	target := fmt.Sprintf("%s:%s", build.Repo, tag)
	return exec.Command(buildahExe, "--storage-driver", "vfs", "push", target)
}

func commandRmi(tag string) *exec.Cmd {
	return exec.Command(buildahExe, "--storage-driver", "vfs", "rmi", tag)
}

func trace(cmd *exec.Cmd) {
	fmt.Fprintf(os.Stdout, "+ %s\n", strings.Join(cmd.Args, " "))
}
