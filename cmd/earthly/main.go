package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	_ "net/http/pprof" // enable pprof handlers on net/http listener
	"os"
	"os/signal"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/earthly/earthly/analytics"
	"github.com/earthly/earthly/autocomplete"
	"github.com/earthly/earthly/buildcontext"
	"github.com/earthly/earthly/buildcontext/provider"
	"github.com/earthly/earthly/builder"
	"github.com/earthly/earthly/buildkitd"
	"github.com/earthly/earthly/cleanup"
	"github.com/earthly/earthly/config"
	"github.com/earthly/earthly/conslogging"
	debuggercommon "github.com/earthly/earthly/debugger/common"
	"github.com/earthly/earthly/debugger/terminal"
	"github.com/earthly/earthly/docker2earthly"
	"github.com/earthly/earthly/domain"
	"github.com/earthly/earthly/earthfile2llb"
	"github.com/earthly/earthly/fileutil"
	"github.com/earthly/earthly/llbutil"
	"github.com/earthly/earthly/secretsclient"
	"github.com/earthly/earthly/termutil"
	"github.com/earthly/earthly/variables"

	humanize "github.com/dustin/go-humanize"
	"github.com/fatih/color"
	"github.com/joho/godotenv"
	"github.com/moby/buildkit/client"
	_ "github.com/moby/buildkit/client/connhelper/dockercontainer" // Load "docker-container://" helper.
	"github.com/moby/buildkit/client/llb"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth/authprovider"
	"github.com/moby/buildkit/session/localhost/localhostprovider"
	"github.com/moby/buildkit/session/sshforward/sshprovider"
	"github.com/moby/buildkit/util/entitlements"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/seehuhn/password"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/sync/errgroup"
)

var dotEnvPath = ".env"

type earthlyApp struct {
	cliApp      *cli.App
	console     conslogging.ConsoleLogger
	cfg         *config.Config
	sessionID   string
	commandName string
	cliFlags
}

type cliFlags struct {
	platformsStr           cli.StringSlice
	buildArgs              cli.StringSlice
	secrets                cli.StringSlice
	secretFiles            cli.StringSlice
	artifactMode           bool
	imageMode              bool
	pull                   bool
	push                   bool
	ci                     bool
	noOutput               bool
	noCache                bool
	pruneAll               bool
	pruneReset             bool
	buildkitdSettings      buildkitd.Settings
	allowPrivileged        bool
	enableProfiler         bool
	buildkitHost           string
	buildkitdImage         string
	remoteCache            string
	maxRemoteCache         bool
	saveInlineCache        bool
	useInlineCache         bool
	configPath             string
	gitUsernameOverride    string
	gitPasswordOverride    string
	interactiveDebugging   bool
	sshAuthSock            string
	verbose                bool
	debug                  bool
	homebrewSource         string
	email                  string
	token                  string
	password               string
	disableNewLine         bool
	secretFile             string
	secretStdin            bool
	apiServer              string
	writePermission        bool
	registrationPublicKey  string
	dockerfilePath         string
	earthfilePath          string
	earthfileFinalImage    string
	expiry                 string
	termsConditionsPrivacy bool
	authToken              string
	noFakeDep              bool
}

var (
	// DefaultBuildkitdImage is the default buildkitd image to use.
	DefaultBuildkitdImage string

	// Version is the version of this CLI app.
	Version string

	// GitSha contains the git sha used to build this app
	GitSha string
)

func profhandler() {
	addr := "127.0.0.1:6060"
	fmt.Printf("listening for pprof on %s\n", addr)
	http.ListenAndServe(addr, nil)
}

func main() {
	startTime := time.Now()
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
	defer func() {
		signal.Stop(c)
		cancel()
	}()
	go func() {
		receivedSignal := false
		for {
			select {
			case sig := <-c:
				cancel()
				if receivedSignal {
					// This is the second time we have received a signal. Quit immediately.
					fmt.Printf("Received second signal %s. Forcing exit.\n", sig.String())
					os.Exit(9)
				}
				receivedSignal = true
				fmt.Printf("Received signal %s. Cleaning up before exiting...\n", sig.String())
				go func() {
					// Wait for 30 seconds before forcing an exit.
					time.Sleep(30 * time.Second)
					fmt.Printf("Timed out cleaning up. Forcing exit.\n")
					os.Exit(9)
				}()
			}
		}
	}()
	// Occasional spurious warnings show up - these are coming from imported libraries. Discard them.
	logrus.StandardLogger().Out = ioutil.Discard

	// Load .env into current global env's. This is mainly for applying Earthly settings.
	// Separate call is made for build args and secrets.
	if fileutil.FileExists(dotEnvPath) {
		err := godotenv.Load(dotEnvPath)
		if err != nil {
			fmt.Printf("Error loading dot-env file %s: %s\n", dotEnvPath, err.Error())
			os.Exit(1)
		}
	}

	colorMode := conslogging.AutoColor
	_, forceColor := os.LookupEnv("FORCE_COLOR")
	if forceColor {
		colorMode = conslogging.ForceColor
		color.NoColor = false
	}
	_, noColor := os.LookupEnv("NO_COLOR")
	if noColor {
		colorMode = conslogging.NoColor
		color.NoColor = true
	}

	padding := conslogging.DefaultPadding
	customPadding, ok := os.LookupEnv("EARTHLY_TARGET_PADDING")
	if ok {
		targetPadding, err := strconv.Atoi(customPadding)
		if err == nil {
			padding = targetPadding
		}
	}

	_, fullTarget := os.LookupEnv("EARTHLY_FULL_TARGET")
	if fullTarget {
		padding = conslogging.NoPadding
	}

	app := newEarthlyApp(ctx, conslogging.Current(colorMode, padding))
	app.autoComplete()

	exitCode := app.run(ctx, os.Args)
	// app.cfg will be nil when a user runs `earthly --version`;
	// however in all other regular commands app.cfg will be set in app.Before
	if app.cfg != nil && !app.cfg.Global.DisableAnalytics {
		ctxTimeout, cancel := context.WithTimeout(ctx, time.Millisecond*500)
		defer cancel()
		displayErrors := app.verbose
		analytics.CollectAnalytics(ctxTimeout, app.apiServer, displayErrors, Version, GitSha, app.commandName, exitCode, time.Since(startTime))
	}
	os.Exit(exitCode)
}

func getVersion() string {
	var isRelease = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
	if isRelease.MatchString(Version) {
		return Version
	}
	return fmt.Sprintf("%s-%s", Version, GitSha)
}

func getBinaryName() string {
	if len(os.Args) == 0 {
		return "earthly"
	}
	binPath := os.Args[0] // can't use os.Executable() here; because it will give us earthly if executed via the earth symlink
	baseName := path.Base(binPath)
	return baseName
}

func newEarthlyApp(ctx context.Context, console conslogging.ConsoleLogger) *earthlyApp {
	sessionIDBytes := make([]byte, 64)
	_, err := rand.Read(sessionIDBytes)
	if err != nil {
		panic(err)
	}
	app := &earthlyApp{
		cliApp:    cli.NewApp(),
		console:   console,
		sessionID: base64.StdEncoding.EncodeToString(sessionIDBytes),
		cliFlags: cliFlags{
			buildkitdSettings: buildkitd.Settings{},
		},
	}

	earthly := getBinaryName()

	app.cliApp.Usage = "A build automation tool for the container era"
	app.cliApp.UsageText = "\t" + earthly + " [options] <target-ref>\n" +
		"\n" +
		"   \t" + earthly + " [options] --image <target-ref>\n" +
		"\n" +
		"   \t" + earthly + " [options] --artifact <artifact-ref> [<dest-path>]\n" +
		"\n" +
		"   \t" + earthly + " [options] command [command options]\n" +
		"\n" +
		"Executes Earthly builds. For more information see https://docs.earthly.dev/earthly-command.\n" +
		"To get started with using Earthly, check out the getting started guide at https://docs.earthly.dev/guides/basics."
	app.cliApp.UseShortOptionHandling = true
	app.cliApp.Action = app.actionBuild
	app.cliApp.Version = getVersion()
	app.cliApp.Flags = []cli.Flag{
		&cli.StringSliceFlag{
			Name:    "platform",
			EnvVars: []string{"EARTHLY_PLATFORMS"},
			Usage:   "Specify the target platform to build for *experimental*",
			Value:   &app.platformsStr,
		},
		&cli.StringSliceFlag{
			Name:    "build-arg",
			EnvVars: []string{"EARTHLY_BUILD_ARGS"},
			Usage:   "A build arg override, specified as <key>=[<value>]",
			Value:   &app.buildArgs,
		},
		&cli.StringSliceFlag{
			Name:    "secret",
			Aliases: []string{"s"},
			EnvVars: []string{"EARTHLY_SECRETS"},
			Usage:   "A secret override, specified as <key>=[<value>]",
			Value:   &app.secrets,
		},
		&cli.StringSliceFlag{
			Name:    "secret-file",
			EnvVars: []string{"EARTHLY_SECRET_FILES"},
			Usage:   "A secret override, specified as <key>=<path>",
			Value:   &app.secretFiles,
		},
		&cli.BoolFlag{
			Name:        "artifact",
			Aliases:     []string{"a"},
			Usage:       "Output only specified artifact",
			Destination: &app.artifactMode,
		},
		&cli.BoolFlag{
			Name:        "image",
			Usage:       "Output only docker image of the specified target",
			Destination: &app.imageMode,
		},
		&cli.BoolFlag{
			Name:        "pull",
			EnvVars:     []string{"EARTHLY_PULL"},
			Usage:       "Force pull any referenced Docker images",
			Destination: &app.pull,
		},
		&cli.BoolFlag{
			Name:        "push",
			EnvVars:     []string{"EARTHLY_PUSH"},
			Usage:       "Push docker images and execute RUN --push commands",
			Destination: &app.push,
		},
		&cli.BoolFlag{
			Name:        "ci",
			EnvVars:     []string{"EARTHLY_CI"},
			Usage:       wrap("Execute in CI mode (implies --use-inline-cache --save-inline-cache --no-output)", "*experimental*"),
			Destination: &app.ci,
		},
		&cli.BoolFlag{
			Name:        "no-output",
			EnvVars:     []string{"EARTHLY_NO_OUTPUT"},
			Usage:       wrap("Do not output artifacts or images", "(using --push is still allowed)"),
			Destination: &app.noOutput,
		},
		&cli.BoolFlag{
			Name:        "no-cache",
			EnvVars:     []string{"EARTHLY_NO_CACHE"},
			Usage:       "Do not use cache while building",
			Destination: &app.noCache,
		},
		&cli.StringFlag{
			Name:        "config",
			Value:       defaultConfigPath(),
			EnvVars:     []string{"EARTHLY_CONFIG"},
			Usage:       "Path to config file",
			Destination: &app.configPath,
		},
		&cli.StringFlag{
			Name:        "ssh-auth-sock",
			Value:       os.Getenv("SSH_AUTH_SOCK"),
			EnvVars:     []string{"EARTHLY_SSH_AUTH_SOCK"},
			Usage:       wrap("The SSH auth socket to use for ssh-agent forwarding", ""),
			Destination: &app.sshAuthSock,
		},
		&cli.StringFlag{
			Name:        "auth-token",
			EnvVars:     []string{"EARTHLY_TOKEN"},
			Usage:       "Force Earthly account login to authenticate with supplied token",
			Destination: &app.authToken,
		},
		&cli.StringFlag{
			Name:        "git-username",
			EnvVars:     []string{"GIT_USERNAME"},
			Usage:       "The git username to use for git HTTPS authentication",
			Destination: &app.gitUsernameOverride,
		},
		&cli.StringFlag{
			Name:        "git-password",
			EnvVars:     []string{"GIT_PASSWORD"},
			Usage:       "The git password to use for git HTTPS authentication",
			Destination: &app.gitPasswordOverride,
		},
		&cli.StringFlag{
			Name:        "git-url-instead-of",
			Value:       "",
			EnvVars:     []string{"GIT_URL_INSTEAD_OF"},
			Usage:       wrap("Rewrite git URLs of a certain pattern. Similar to git-config url.", "<base>.insteadOf (https://git-scm.com/docs/git-config#Documentation/git-config.txt-urlltbasegtinsteadOf).", "Multiple values can be separated by commas. Format: <base>=<instead-of>[,...]. ", "For example: 'https://github.com/=git@github.com:'"),
			Destination: &app.buildkitdSettings.GitURLInsteadOf,
		},
		&cli.BoolFlag{
			Name:        "allow-privileged",
			Aliases:     []string{"P"},
			EnvVars:     []string{"EARTHLY_ALLOW_PRIVILEGED"},
			Usage:       "Allow build to use the --privileged flag in RUN commands",
			Destination: &app.allowPrivileged,
		},
		&cli.BoolFlag{
			Name:        "profiler",
			EnvVars:     []string{"EARTHLY_PROFILER"},
			Usage:       "Enable the profiler",
			Destination: &app.enableProfiler,
			Hidden:      true, // Dev purposes only.
		},
		&cli.StringFlag{
			Name:        "buildkit-host",
			EnvVars:     []string{"EARTHLY_BUILDKIT_HOST"},
			Usage:       wrap("The URL to use for connecting to a buildkit host. ", "If empty, earthly will attempt to start a buildkitd instance via docker run"),
			Destination: &app.buildkitHost,
		},
		&cli.IntFlag{
			Name:        "buildkit-cache-size-mb",
			Value:       10000,
			EnvVars:     []string{"EARTHLY_BUILDKIT_CACHE_SIZE_MB"},
			Usage:       "The total size of the buildkit cache, in MB",
			Destination: &app.buildkitdSettings.CacheSizeMb,
		},
		&cli.StringFlag{
			Name:        "buildkit-image",
			Value:       DefaultBuildkitdImage,
			EnvVars:     []string{"EARTHLY_BUILDKIT_IMAGE"},
			Usage:       "The docker image to use for the buildkit daemon",
			Destination: &app.buildkitdImage,
		},
		&cli.StringFlag{
			Name:        "remote-cache",
			EnvVars:     []string{"EARTHLY_REMOTE_CACHE"},
			Usage:       "A remote docker image tag use as explicit cache *experimental*",
			Destination: &app.remoteCache,
		},
		&cli.BoolFlag{
			Name:        "max-remote-cache",
			EnvVars:     []string{"EARTHLY_MAX_REMOTE_CACHE"},
			Usage:       "Saves all intermediate images too in the remove cache *experimental*",
			Destination: &app.maxRemoteCache,
		},
		&cli.BoolFlag{
			Name:        "save-inline-cache",
			EnvVars:     []string{"EARTHLY_SAVE_INLINE_CACHE"},
			Usage:       "Enable cache inlining when pushing images *experimental*",
			Destination: &app.saveInlineCache,
		},
		&cli.BoolFlag{
			Name:        "use-inline-cache",
			EnvVars:     []string{"EARTHLY_USE_INLINE_CACHE"},
			Usage:       wrap("Attempt to use any inline cache that may have been previously pushed ", "uses image tags referenced by SAVE IMAGE --push or SAVE IMAGE --cache-from", "*experimental*"),
			Destination: &app.useInlineCache,
		},
		&cli.BoolFlag{
			Name:        "interactive",
			Aliases:     []string{"i"},
			EnvVars:     []string{"EARTHLY_INTERACTIVE"},
			Usage:       "Enable interactive debugging",
			Destination: &app.interactiveDebugging,
		},
		&cli.BoolFlag{
			Name:        "verbose",
			Aliases:     []string{"V"},
			EnvVars:     []string{"EARTHLY_VERBOSE"},
			Usage:       "Enable verbose logging",
			Destination: &app.verbose,
		},
		&cli.BoolFlag{
			Name:        "debug",
			Aliases:     []string{"D"},
			EnvVars:     []string{"EARTHLY_DEBUG"},
			Usage:       "Enable debug mode",
			Destination: &app.debug,
		},
		&cli.StringFlag{
			Name:        "server",
			Value:       "https://api.earthly.dev",
			EnvVars:     []string{"EARTHLY_SERVER"},
			Usage:       "API server override for dev purposes",
			Destination: &app.apiServer,
			Hidden:      true, // Internal.
		},
		&cli.BoolFlag{
			Name:        "no-fake-dep",
			EnvVars:     []string{"EARTHLY_NO_FAKE_DEP"},
			Usage:       "Internal feature flag for fake-dep",
			Destination: &app.noFakeDep,
			Hidden:      true, // Internal.
		},
	}

	app.cliApp.Commands = []*cli.Command{
		{
			Name:        "bootstrap",
			Usage:       "Bootstraps earthly bash autocompletion",
			Description: "Performs initial earthly bootstrapping for bash autocompletion",
			Action:      app.actionBootstrap,
			Flags: []cli.Flag{
				&cli.StringFlag{
					Name:        "source",
					Usage:       "output source file (for use in homebrew install)",
					Hidden:      true, // only meant for use with homebrew formula
					Destination: &app.homebrewSource,
				},
			},
		},
		{
			Name:        "docker2earthly",
			Usage:       "Convert a Dockerfile into Earthfile",
			Description: "Converts an existing dockerfile into an Earthfile",
			Hidden:      true, // Experimental.
			Action:      app.actionDocker2Earthly,
			Flags: []cli.Flag{
				&cli.StringFlag{
					Name:        "dockerfile",
					Usage:       "Path to dockerfile input, or - for stdin",
					Value:       "Dockerfile",
					Destination: &app.dockerfilePath,
				},
				&cli.StringFlag{
					Name:        "earthfile",
					Usage:       "Path to earthfile output, or - for stdout",
					Value:       "Earthfile",
					Destination: &app.earthfilePath,
				},
				&cli.StringFlag{
					Name:        "tag",
					Usage:       "Name and tag for the built image; formatted as 'name:tag'",
					Destination: &app.earthfileFinalImage,
				},
			},
		},
		{
			Name:  "org",
			Usage: "Earthly organization administration *experimental*",
			Subcommands: []*cli.Command{
				{
					Name:      "create",
					Usage:     "Create a new organization",
					UsageText: "earthly [options] org create <org-name>",
					Action:    app.actionOrgCreate,
				},
				{
					Name:      "list",
					Usage:     "List organizations you belong to",
					UsageText: "earthly [options] org list",
					Action:    app.actionOrgList,
				},
				{
					Name:      "list-permissions",
					Usage:     "List permissions and membership of an organization",
					UsageText: "earthly [options] org list-permissions <org-name>",
					Action:    app.actionOrgListPermissions,
				},
				{
					Name:      "invite",
					Usage:     "Invite accounts to your organization",
					UsageText: "earthly [options] org invite [options] <path> <email> [<email> ...]",
					Action:    app.actionOrgInvite,
					Flags: []cli.Flag{
						&cli.BoolFlag{
							Name:        "write",
							Usage:       "Grant write permissions in addition to read",
							Destination: &app.writePermission,
						},
					},
				},
				{
					Name:      "revoke",
					Usage:     "Remove accounts from your organization",
					UsageText: "earthly [options] org revoke <path> <email> [<email> ...]",
					Action:    app.actionOrgRevoke,
				},
			},
		},
		{
			Name:        "secrets",
			Usage:       "Earthly secrets",
			Description: "Manage cloud secrets *experimental*",
			Subcommands: []*cli.Command{
				{
					Name:  "set",
					Usage: "Stores a secret in the secrets store",
					UsageText: "earthly [options] secrets set <path> <value>\n" +
						"   earthly [options] secrets set --file <local-path> <path>\n" +
						"   earthly [options] secrets set --file <local-path> <path>",
					Action: app.actionSecretsSet,
					Flags: []cli.Flag{
						&cli.StringFlag{
							Name:        "file",
							Aliases:     []string{"f"},
							Usage:       "Stores secret stored in file",
							Destination: &app.secretFile,
						},
						&cli.BoolFlag{
							Name:        "stdin",
							Aliases:     []string{"i"},
							Usage:       "Stores secret read from stdin",
							Destination: &app.secretStdin,
						},
					},
				},
				{
					Name:      "get",
					Action:    app.actionSecretsGet,
					Usage:     "Retrieve a secret from the secrets store",
					UsageText: "earthly [options] secrets get [options] <path>",
					Flags: []cli.Flag{
						&cli.BoolFlag{
							Aliases:     []string{"n"},
							Usage:       "Disable newline at the end of the secret",
							Destination: &app.disableNewLine,
						},
					},
				},
				{
					Name:      "ls",
					Usage:     "List secrets in the secrets store",
					UsageText: "earthly [options] secrets ls [<path>]",
					Action:    app.actionSecretsList,
				},
				{
					Name:      "rm",
					Usage:     "Removes a secret from the secrets store",
					UsageText: "earthly [options] secrets rm <path>",
					Action:    app.actionSecretsRemove,
				},
			},
		},
		{
			Name:  "account",
			Usage: "Create or manage an Earthly account *experimental*",
			Subcommands: []*cli.Command{
				{
					Name:        "register",
					Usage:       "Register for an Earthly account",
					Description: "Register for an Earthly account",
					UsageText: "first, request a token with:\n" +
						"     earthly [options] account register --email <email>\n" +
						"\n" +
						"   then check your email to retrieve the token, then continue by running:\n" +
						"     earthly [options] account register --email <email> --token <token> [options]",
					Action: app.actionRegister,
					Flags: []cli.Flag{
						&cli.StringFlag{
							Name:        "email",
							Usage:       "Email address",
							Destination: &app.email,
						},
						&cli.StringFlag{
							Name:        "token",
							Usage:       "Email verification token",
							Destination: &app.token,
						},
						&cli.StringFlag{
							Name:        "password",
							EnvVars:     []string{"EARTHLY_PASSWORD"},
							Usage:       "Specify password on the command line instead of interactively being asked",
							Destination: &app.password,
						},
						&cli.StringFlag{
							Name:        "public-key",
							EnvVars:     []string{"EARTHLY_PUBLIC_KEY"},
							Usage:       "Path to public key to register",
							Destination: &app.registrationPublicKey,
						},
						&cli.BoolFlag{
							Name:        "accept-terms-of-service-privacy",
							EnvVars:     []string{"EARTHLY_ACCEPT_TERMS_OF_SERVICE_PRIVACY"},
							Usage:       "Accept the Terms & Conditions, and Privacy Policy",
							Destination: &app.termsConditionsPrivacy,
						},
					},
				},
				{
					Name:        "login",
					Usage:       "Login to an Earthly account",
					Description: "Login to an Earthly account",
					UsageText: "earthly [options] account login\n" +
						"   earthly [options] account login --email <email>\n" +
						"   earthly [options] account login --email <email> --password <password>\n" +
						"   earthly [options] account login --token <token>\n",
					Action: app.actionAccountLogin,
					Flags: []cli.Flag{
						&cli.StringFlag{
							Name:        "email",
							Usage:       "Email address",
							Destination: &app.email,
						},
						&cli.StringFlag{
							Name:        "token",
							Usage:       "Authentication token",
							Destination: &app.token,
						},
						&cli.StringFlag{
							Name:        "password",
							EnvVars:     []string{"EARTHLY_PASSWORD"},
							Usage:       "Specify password on the command line instead of interactively being asked",
							Destination: &app.password,
						},
					},
				},
				{
					Name:        "logout",
					Usage:       "Logout of an Earthly account",
					Description: "Logout of an Earthly account; this has no effect for ssh-based authentication",
					Action:      app.actionAccountLogout,
				},
				{
					Name:      "list-keys",
					Usage:     "List associated public keys used for authentication",
					UsageText: "earthly [options] account list-keys",
					Action:    app.actionAccountListKeys,
				},
				{
					Name:      "add-key",
					Usage:     "Associate a new public key with your account",
					UsageText: "earthly [options] add-key [<key>]",
					Action:    app.actionAccountAddKey,
				},
				{
					Name:      "remove-key",
					Usage:     "Removes an existing public key from your account",
					UsageText: "earthly [options] remove-key <key>",
					Action:    app.actionAccountRemoveKey,
				},
				{
					Name:      "list-tokens",
					Usage:     "List associated tokens used for authentication",
					UsageText: "earthly [options] account list-tokens",
					Action:    app.actionAccountListTokens,
				},
				{
					Name:      "create-token",
					Usage:     "Create a new authentication token for your account",
					UsageText: "earthly [options] account create-token [options] <token name>",
					Action:    app.actionAccountCreateToken,
					Flags: []cli.Flag{
						&cli.BoolFlag{
							Name:        "write",
							Usage:       "Grant write permissions in addition to read",
							Destination: &app.writePermission,
						},
						&cli.StringFlag{
							Name:        "expiry",
							Usage:       "Set token expiry date in the form YYYY-MM-DD or never (default 1year)",
							Destination: &app.expiry,
						},
					},
				},
				{
					Name:      "remove-token",
					Usage:     "Remove an authentication token from your account",
					UsageText: "earthly [options] account remove-token <token>",
					Action:    app.actionAccountRemoveToken,
				},
			},
		},
		{
			Name:        "debug",
			Usage:       "Print debug information about an Earthfile",
			Description: "Print debug information about an Earthfile",
			ArgsUsage:   "[<path>]",
			Hidden:      true, // Dev purposes only.
			Action:      app.actionDebug,
		},
		{
			Name:        "prune",
			Usage:       "Prune Earthly build cache",
			Description: "Prune Earthly build cache",
			Action:      app.actionPrune,
			Flags: []cli.Flag{
				&cli.BoolFlag{
					Name:        "all",
					Aliases:     []string{"a"},
					EnvVars:     []string{"EARTHLY_PRUNE_ALL"},
					Usage:       "Prune all cache",
					Destination: &app.pruneAll,
				},
				&cli.BoolFlag{
					Name:        "reset",
					EnvVars:     []string{"EARTHLY_PRUNE_RESET"},
					Usage:       "Reset cache entirely by wiping cache dir",
					Destination: &app.pruneReset,
				},
			},
		},
	}

	app.cliApp.Before = app.before
	return app
}

func wrap(s ...string) string {
	return strings.Join(s, "\n\t")
}

func (app *earthlyApp) before(context *cli.Context) error {
	if app.enableProfiler {
		go profhandler()
	}

	if context.IsSet("config") {
		app.console.Printf("loading config values from %q\n", app.configPath)
	}

	yamlData, err := ioutil.ReadFile(app.configPath)
	if os.IsNotExist(err) && !context.IsSet("config") {
		yamlData = []byte{}
	} else if err != nil {
		return errors.Wrapf(err, "failed to read from %s", app.configPath)
	}

	app.cfg, err = config.ParseConfigFile(yamlData)
	if err != nil {
		return errors.Wrapf(err, "failed to parse %s", app.configPath)
	}

	if app.cfg.Git == nil {
		app.cfg.Git = map[string]config.GitConfig{}
	}

	err = app.processDeprecatedCommandOptions(context, app.cfg)
	if err != nil {
		return err
	}

	// command line option overrides the config which overrides the default value
	if !context.IsSet("buildkit-image") && app.cfg.Global.BuildkitImage != "" {
		app.buildkitdImage = app.cfg.Global.BuildkitImage
	}

	if !fileutil.DirExists(app.cfg.Global.RunPath) {
		err := os.MkdirAll(app.cfg.Global.RunPath, 0755)
		if err != nil {
			return errors.Wrapf(err, "failed to create run directory %s", app.cfg.Global.RunPath)
		}
	}

	app.buildkitdSettings.DebuggerPort = app.cfg.Global.DebuggerPort
	app.buildkitdSettings.RunDir = app.cfg.Global.RunPath
	app.buildkitdSettings.AdditionalArgs = app.cfg.Global.BuildkitAdditionalArgs

	return nil
}

func (app *earthlyApp) warnIfEarth() {
	if len(os.Args) == 0 {
		return
	}
	binPath := os.Args[0] // can't use os.Executable() here; because it will give us earthly if executed via the earth symlink

	baseName := path.Base(binPath)
	if baseName == "earth" {
		app.console.Warnf("Warning: the earth binary has been renamed to earthly; the earth command is currently symlinked, but is deprecated and will one day be removed.")

		absPath, err := filepath.Abs(binPath)
		if err != nil {
			return
		}
		earthlyPath := path.Join(path.Dir(absPath), "earthly")
		if fileutil.FileExists(earthlyPath) {
			app.console.Warnf("Once you are ready to switch over to earthly, you can `rm %s`", absPath)
		}
	}
}

func (app *earthlyApp) processDeprecatedCommandOptions(context *cli.Context, cfg *config.Config) error {
	app.warnIfEarth()

	if cfg.Global.CachePath != "" {
		app.console.Warnf("Warning: the setting cache_path is now obsolete and will be ignored")
	}

	// command line overrides the config file
	if app.gitUsernameOverride != "" || app.gitPasswordOverride != "" {
		app.console.Warnf("Warning: the --git-username and --git-password command flags are deprecated and are now configured in the ~/.earthly/config.yml file under the git section; see https://docs.earthly.dev/earthly-config for reference.\n")
		if _, ok := cfg.Git["github.com"]; !ok {
			cfg.Git["github.com"] = config.GitConfig{}
		}
		if _, ok := cfg.Git["gitlab.com"]; !ok {
			cfg.Git["gitlab.com"] = config.GitConfig{}
		}

		for k, v := range cfg.Git {
			v.Auth = "https"
			if app.gitUsernameOverride != "" {
				v.User = app.gitUsernameOverride
			}
			if app.gitPasswordOverride != "" {
				v.Password = app.gitPasswordOverride
			}
			cfg.Git[k] = v
		}
	}

	if context.IsSet("git-url-instead-of") {
		app.console.Warnf("Warning: the --git-url-instead-of command flag is deprecated and is now configured in the ~/.earthly/config.yml file under the git global url_instead_of setting; see https://docs.earthly.dev/earthly-config for reference.\n")
	} else {
		if gitGlobal, ok := cfg.Git["global"]; ok {
			if gitGlobal.GitURLInsteadOf != "" {
				app.buildkitdSettings.GitURLInsteadOf = gitGlobal.GitURLInsteadOf
			}
		}
	}

	if context.IsSet("buildkit-cache-size-mb") {
		app.console.Warnf("Warning: the --buildkit-cache-size-mb command flag is deprecated and is now configured in the ~/.earthly/config.yml file under the buildkit_cache_size setting; see https://docs.earthly.dev/earthly-config for reference.\n")
	} else {
		app.buildkitdSettings.CacheSizeMb = cfg.Global.BuildkitCacheSizeMb
	}

	return nil
}

// to enable autocomplete, enter
// complete -o nospace -C "/path/to/earthly" earthly
func (app *earthlyApp) autoComplete() {
	_, found := os.LookupEnv("COMP_LINE")
	if !found {
		return
	}

	err := app.autoCompleteImp()
	if err != nil {
		errToLog := err
		homeDir, err := os.UserHomeDir()
		if err != nil {
			os.Exit(1)
		}
		logDir := filepath.Join(homeDir, ".earthly")
		logFile := filepath.Join(logDir, "autocomplete.log")
		err = os.MkdirAll(logDir, 0755)
		if err != nil {
			os.Exit(1)
		}
		f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0755)
		if err != nil {
			os.Exit(1)
		}
		fmt.Fprintf(f, "error during autocomplete: %s\n", errToLog)
		os.Exit(1)
	}
	os.Exit(0)
}

func (app *earthlyApp) autoCompleteImp() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovered panic in autocomplete %s: %s", r, debug.Stack())
		}
	}()

	compLine := os.Getenv("COMP_LINE")   // full command line
	compPoint := os.Getenv("COMP_POINT") // where the cursor is

	compPointInt, err := strconv.ParseUint(compPoint, 10, 64)
	if err != nil {
		return err
	}

	potentials, err := autocomplete.GetPotentials(compLine, int(compPointInt), app.cliApp)
	if err != nil {
		return err
	}
	for _, p := range potentials {
		fmt.Printf("%s\n", p)
	}

	return err
}

const bashCompleteEntry = "complete -o nospace -C '/usr/local/bin/earthly' earthly\n"

func (app *earthlyApp) insertBashCompleteEntry() error {
	var path string
	if runtime.GOOS == "darwin" {
		path = "/usr/local/etc/bash_completion.d/earthly"
	} else {
		path = "/usr/share/bash-completion/completions/earthly"
	}
	dirPath := filepath.Dir(path)

	if !fileutil.DirExists(dirPath) {
		fmt.Fprintf(os.Stderr, "Warning: unable to enable bash-completion: %s does not exist\n", dirPath)
		return nil // bash-completion isn't available, silently fail.
	}

	if fileutil.FileExists(path) {
		return nil // file already exists, don't update it.
	}

	// create the completion file
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write([]byte(bashCompleteEntry))
	return err
}

func (app *earthlyApp) deleteZcompdump() error {
	var homeDir string
	sudoUser, found := os.LookupEnv("SUDO_USER")
	if !found {
		var err error
		homeDir, err = os.UserHomeDir()
		if err != nil {
			return errors.Wrapf(err, "failed to lookup current user home dir")
		}
	} else {
		currentUser, err := user.Lookup(sudoUser)
		if err != nil {
			return errors.Wrapf(err, "failed to lookup user %s", sudoUser)
		}
		homeDir = currentUser.HomeDir
	}
	files, err := ioutil.ReadDir(homeDir)
	if err != nil {
		return errors.Wrapf(err, "failed to read dir %s", homeDir)
	}
	for _, f := range files {
		if strings.HasPrefix(f.Name(), ".zcompdump") {
			path := filepath.Join(homeDir, f.Name())
			err := os.Remove(path)
			if err != nil {
				return errors.Wrapf(err, "failed to remove %s", path)
			}
		}
	}
	return nil
}

const zshCompleteEntry = `#compdef _earth earthly

function _earth {
    autoload -Uz bashcompinit
    bashcompinit
    complete -o nospace -C '/usr/local/bin/earthly' earthly
}
`

// If debugging this, it might be required to run `rm ~/.zcompdump*` to remove the cache
func (app *earthlyApp) insertZSHCompleteEntry() error {
	// should be the same on linux and macOS
	path := "/usr/local/share/zsh/site-functions/_earth"
	dirPath := filepath.Dir(path)

	if !fileutil.DirExists(dirPath) {
		fmt.Fprintf(os.Stderr, "Warning: unable to enable zsh-completion: %s does not exist\n", dirPath)
		return nil // zsh-completion isn't available, silently fail.
	}

	if fileutil.FileExists(path) {
		return nil // file already exists, don't update it.
	}

	// create the completion file
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.Write([]byte(zshCompleteEntry))
	if err != nil {
		return err
	}

	return app.deleteZcompdump()
}

func (app *earthlyApp) run(ctx context.Context, args []string) int {
	err := app.cliApp.RunContext(ctx, args)

	rpcRegex := regexp.MustCompile(`(?U)rpc error: code = .+ desc = .+:\s`)
	if err != nil {
		if strings.Contains(err.Error(), "security.insecure is not allowed") {
			app.console.Warnf("Error: --allow-privileged (-P) flag is required\n")
		} else if strings.Contains(err.Error(), "failed to fetch remote") {
			app.console.Warnf("Error: %v\n", err)
			app.console.Printf(
				"Check your git auth settings.\n" +
					"Did you ssh-add today? Need to configure ~/.earthly/config.yml?\n" +
					"For more information see https://docs.earthly.dev/guides/auth\n")
		} else if !app.verbose && rpcRegex.MatchString(err.Error()) {
			baseErr := errors.Cause(err)
			baseErrMsg := rpcRegex.ReplaceAll([]byte(baseErr.Error()), []byte(""))

			app.console.Warnf("Error: %v\n", string(baseErrMsg))
		} else {
			app.console.Warnf("Error: %v\n", err)
		}
		if errors.Is(err, context.Canceled) {
			return 2
		}
		return 1
	}
	return 0
}

// apply heuristics to see if binary is a version of earthly
func isEarthlyBinary(path string) bool {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return false
	}
	if !bytes.Contains(data, []byte("docs.earthly.dev")) {
		return false
	}
	if !bytes.Contains(data, []byte("api.earthly.dev")) {
		return false
	}
	if !bytes.Contains(data, []byte("Earthfile")) {
		return false
	}
	return true
}

func symlinkEarthlyToEarth() error {
	binPath, err := os.Executable()
	if err != nil {
		return errors.Wrap(err, "failed to get current executable path")
	}

	baseName := path.Base(binPath)
	if baseName != "earthly" {
		return nil
	}

	earthPath := path.Join(path.Dir(binPath), "earth")

	if !fileutil.FileExists(earthPath) && termutil.IsTTY() {
		return nil // legacy earth binary doesn't exist, don't create it (unless we're under a non-tty system e.g. CI)
	}

	if !isEarthlyBinary(earthPath) {
		return nil // file exists but is not an earthly binary, leave it alone.
	}

	// otherwise legacy earth command has been detected, remove it and symlink
	// to the new earthly command.
	err = os.Remove(earthPath)
	if err != nil {
		return errors.Wrapf(err, "failed to remove old install at %s", earthPath)
	}
	err = os.Symlink(binPath, earthPath)
	if err != nil {
		return errors.Wrapf(err, "failed to symlink %s to %s", binPath, earthPath)
	}
	return nil
}

func (app *earthlyApp) actionBootstrap(c *cli.Context) error {
	app.commandName = "bootstrap"
	switch app.homebrewSource {
	case "bash":
		fmt.Printf(bashCompleteEntry)
		return nil
	case "zsh":
		fmt.Printf(zshCompleteEntry)
		return nil
	case "":
		break
	default:
		return fmt.Errorf("unhandled source %q", app.homebrewSource)
	}

	err := symlinkEarthlyToEarth()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", err.Error())
	}

	err = app.insertBashCompleteEntry()
	if err != nil {
		return err
	}

	err = app.insertZSHCompleteEntry()
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Bootstrapping successful; you may have to restart your shell for autocomplete to get initialized (e.g. run \"exec $SHELL\")\n")

	return nil
}

func promptInput(question string) string {
	fmt.Printf(question)
	rbuf := bufio.NewReader(os.Stdin)
	line, err := rbuf.ReadString('\n')
	if err != nil {
		return ""
	}
	return strings.TrimRight(line, "\n")
}

func (app *earthlyApp) actionOrgCreate(c *cli.Context) error {
	app.commandName = "orgCreate"
	if c.NArg() != 1 {
		return errors.New("invalid number of arguments provided")
	}
	org := c.Args().Get(0)
	sc, err := secretsclient.NewClient(app.apiServer, app.sshAuthSock, app.authToken, app.console.Warnf)
	if err != nil {
		return errors.Wrap(err, "failed to create secretsclient")
	}
	err = sc.CreateOrg(org)
	if err != nil {
		return errors.Wrap(err, "failed to create org")
	}
	return nil
}

func (app *earthlyApp) actionOrgList(c *cli.Context) error {
	app.commandName = "orgList"
	sc, err := secretsclient.NewClient(app.apiServer, app.sshAuthSock, app.authToken, app.console.Warnf)
	if err != nil {
		return errors.Wrap(err, "failed to create secretsclient")
	}
	orgs, err := sc.ListOrgs()
	if err != nil {
		return errors.Wrap(err, "failed to list orgs")
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, org := range orgs {
		fmt.Fprintf(w, "/%s/", org.Name)
		if org.Admin {
			fmt.Fprintf(w, "\tadmin")
		} else {
			fmt.Fprintf(w, "\tmember")
		}
		fmt.Fprintf(w, "\n")
	}
	w.Flush()

	return nil
}

func (app *earthlyApp) actionOrgListPermissions(c *cli.Context) error {
	app.commandName = "orgListPermissions"
	if c.NArg() != 1 {
		return errors.New("invalid number of arguments provided")
	}
	path := c.Args().Get(0)
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	sc, err := secretsclient.NewClient(app.apiServer, app.sshAuthSock, app.authToken, app.console.Warnf)
	if err != nil {
		return errors.Wrap(err, "failed to create secretsclient")
	}
	orgs, err := sc.ListOrgPermissions(path)
	if err != nil {
		return errors.Wrap(err, "failed to list org permissions")
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, org := range orgs {
		fmt.Fprintf(w, "%s\t%s", org.Path, org.User)
		if org.Write {
			fmt.Fprintf(w, "\trw")
		} else {
			fmt.Fprintf(w, "\tr")
		}
		fmt.Fprintf(w, "\n")
	}
	w.Flush()
	return nil
}

func (app *earthlyApp) actionOrgInvite(c *cli.Context) error {
	app.commandName = "orgInvite"
	if c.NArg() < 2 {
		return errors.New("invalid number of arguments provided")
	}
	path := c.Args().Get(0)
	if !strings.HasSuffix(path, "/") {
		return errors.New("invitation paths must end with a slash (/)")
	}

	sc, err := secretsclient.NewClient(app.apiServer, app.sshAuthSock, app.authToken, app.console.Warnf)
	if err != nil {
		return errors.Wrap(err, "failed to create secretsclient")
	}
	userEmail := c.Args().Get(1)
	err = sc.Invite(path, userEmail, app.writePermission)
	if err != nil {
		return errors.Wrap(err, "failed to invite user into org")
	}
	return nil
}

func (app *earthlyApp) actionOrgRevoke(c *cli.Context) error {
	app.commandName = "orgRevoke"
	if c.NArg() < 2 {
		return errors.New("invalid number of arguments provided")
	}
	path := c.Args().Get(0)
	if !strings.HasSuffix(path, "/") {
		return errors.New("revoked paths must end with a slash (/)")
	}

	sc, err := secretsclient.NewClient(app.apiServer, app.sshAuthSock, app.authToken, app.console.Warnf)
	if err != nil {
		return errors.Wrap(err, "failed to create secretsclient")
	}
	userEmail := c.Args().Get(1)
	err = sc.RevokePermission(path, userEmail)
	if err != nil {
		return errors.Wrap(err, "failed to revoke user from org")
	}
	return nil
}

func (app *earthlyApp) actionSecretsList(c *cli.Context) error {
	app.commandName = "secretsList"

	path := "/"
	if c.NArg() > 1 {
		return errors.New("invalid number of arguments provided")
	} else if c.NArg() == 1 {
		path = c.Args().Get(0)
	}
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	sc, err := secretsclient.NewClient(app.apiServer, app.sshAuthSock, app.authToken, app.console.Warnf)
	if err != nil {
		return errors.Wrap(err, "failed to create secretsclient")
	}
	paths, err := sc.List(path)
	if err != nil {
		return errors.Wrap(err, "failed to list secret")
	}
	for _, path := range paths {
		fmt.Println(path)
	}
	return nil
}

func (app *earthlyApp) actionSecretsGet(c *cli.Context) error {
	app.commandName = "secretsGet"
	if c.NArg() != 1 {
		return errors.New("invalid number of arguments provided")
	}
	path := c.Args().Get(0)
	sc, err := secretsclient.NewClient(app.apiServer, app.sshAuthSock, app.authToken, app.console.Warnf)
	if err != nil {
		return errors.Wrap(err, "failed to create secretsclient")
	}
	data, err := sc.Get(path)
	if err != nil {
		return errors.Wrap(err, "failed to get secret")
	}
	fmt.Printf("%s", data)
	if !app.disableNewLine {
		fmt.Printf("\n")
	}
	return nil
}

func (app *earthlyApp) actionSecretsRemove(c *cli.Context) error {
	app.commandName = "secretsRemove"
	if c.NArg() != 1 {
		return errors.New("invalid number of arguments provided")
	}
	path := c.Args().Get(0)
	sc, err := secretsclient.NewClient(app.apiServer, app.sshAuthSock, app.authToken, app.console.Warnf)
	if err != nil {
		return errors.Wrap(err, "failed to create secretsclient")
	}
	err = sc.Remove(path)
	if err != nil {
		return errors.Wrap(err, "failed to remove secret")
	}
	return nil
}

func (app *earthlyApp) actionSecretsSet(c *cli.Context) error {
	app.commandName = "secretsSet"
	var path string
	var value string
	if app.secretFile == "" && !app.secretStdin {
		if c.NArg() != 2 {
			return errors.New("invalid number of arguments provided")
		}
		path = c.Args().Get(0)
		value = c.Args().Get(1)
	} else if app.secretStdin {
		if app.secretFile != "" {
			return errors.New("only one of --file or --stdin can be used at a time")
		}
		if c.NArg() != 1 {
			return errors.New("invalid number of arguments provided")
		}
		path = c.Args().Get(0)
		data, err := ioutil.ReadAll(os.Stdin)
		if err != nil {
			return errors.Wrap(err, "failed to read from stdin")
		}
		value = string(data)
	} else {
		if c.NArg() != 1 {
			return errors.New("invalid number of arguments provided")
		}
		path = c.Args().Get(0)
		data, err := ioutil.ReadFile(app.secretFile)
		if err != nil {
			return errors.Wrapf(err, "failed to read secret from %s", app.secretFile)
		}
		value = string(data)
	}

	sc, err := secretsclient.NewClient(app.apiServer, app.sshAuthSock, app.authToken, app.console.Warnf)
	if err != nil {
		return errors.Wrap(err, "failed to create secretsclient")
	}
	err = sc.Set(path, []byte(value))
	if err != nil {
		return errors.Wrap(err, "failed to set secret")
	}
	return nil
}

func (app *earthlyApp) actionRegister(c *cli.Context) error {
	app.commandName = "secretsRegister"
	if app.email == "" {
		return errors.New("no email given")
	}

	if !strings.Contains(app.email, "@") {
		return errors.New("email is invalid")
	}

	sc, err := secretsclient.NewClient(app.apiServer, app.sshAuthSock, app.authToken, app.console.Warnf)
	if err != nil {
		return errors.Wrap(err, "failed to create secretsclient")
	}

	if app.token == "" {
		err := sc.RegisterEmail(app.email)
		if err != nil {
			return errors.Wrap(err, "failed to register email")
		}
		fmt.Printf("An email has been sent to %q containing a registration token\n", app.email)
		return nil
	}

	var publicKeys []*agent.Key
	if app.sshAuthSock != "" {
		var err error
		publicKeys, err = sc.GetPublicKeys()
		if err != nil {
			return err
		}
	}

	// Our signal handling under main() doesn't cause reading from stdin to cancel
	// as there's no way to pass app.ctx to stdin read calls.
	signal.Reset(syscall.SIGINT, syscall.SIGTERM)

	pword := app.password
	if app.password == "" {
		enteredPassword, err := password.Read("pick a password: ")
		if err != nil {
			return err
		}
		enteredPassword2, err := password.Read("confirm password: ")
		if err != nil {
			return err
		}
		if string(enteredPassword) != string(enteredPassword2) {
			return fmt.Errorf("passwords do not match")
		}
		pword = string(enteredPassword)
	}

	var interactiveAccept bool
	if !app.termsConditionsPrivacy {
		rawAccept := promptInput("I acknowledge Earthly Technologies’ Privacy Policy (https://earthly.dev/privacy-policy) and agree to Earthly Technologies Terms of Service (https://earthly.dev/tos) [y/N]: ")
		if rawAccept == "" {
			rawAccept = "n"
		}
		accept := strings.ToLower(rawAccept)[0]

		interactiveAccept = accept == 'y'
	}
	termsConditionsPrivacy := app.termsConditionsPrivacy || interactiveAccept

	var publicKey string
	if app.registrationPublicKey == "" {
		if len(publicKeys) > 0 {
			fmt.Printf("Which of the following keys do you want to register?\n")
			fmt.Printf("0) none\n")
			for i, key := range publicKeys {
				fmt.Printf("%d) %s\n", i+1, key.String())
			}
			keyNum := promptInput("enter key number (1=default): ")
			if keyNum == "" {
				keyNum = "1"
			}
			i, err := strconv.Atoi(keyNum)
			if err != nil {
				return errors.Wrap(err, "invalid key number")
			}
			if i < 0 || i > len(publicKeys) {
				return fmt.Errorf("invalid key number")
			}
			if i > 0 {
				publicKey = publicKeys[i-1].String()
			}
		}
	} else {
		_, _, _, _, err := ssh.ParseAuthorizedKey([]byte(app.registrationPublicKey))
		if err == nil {
			// supplied public key is valid
			publicKey = app.registrationPublicKey
		} else {
			// otherwise see if it matches the name (Comment) of a key known by the ssh agent
			for _, key := range publicKeys {
				if key.Comment == app.registrationPublicKey {
					publicKey = key.String()
					break
				}
			}
			if publicKey == "" {
				return fmt.Errorf("failed to find key in ssh agent's known keys")
			}
		}
	}

	err = sc.CreateAccount(app.email, app.token, pword, publicKey, termsConditionsPrivacy)
	if err != nil {
		return errors.Wrap(err, "failed to create account")
	}

	fmt.Println("Account registration complete")
	return nil
}

func (app *earthlyApp) actionAccountListKeys(c *cli.Context) error {
	app.commandName = "accountListKeys"
	sc, err := secretsclient.NewClient(app.apiServer, app.sshAuthSock, app.authToken, app.console.Warnf)
	if err != nil {
		return errors.Wrap(err, "failed to create secretsclient")
	}
	keys, err := sc.ListPublicKeys()
	if err != nil {
		return errors.Wrap(err, "failed to list account keys")
	}
	for _, key := range keys {
		fmt.Printf("%s\n", key)
	}
	return nil
}

func (app *earthlyApp) actionAccountAddKey(c *cli.Context) error {
	app.commandName = "accountAddKey"
	sc, err := secretsclient.NewClient(app.apiServer, app.sshAuthSock, app.authToken, app.console.Warnf)
	if err != nil {
		return errors.Wrap(err, "failed to create secretsclient")
	}
	if c.NArg() > 1 {
		for _, k := range c.Args().Slice() {
			err := sc.AddPublickKey(k)
			if err != nil {
				return errors.Wrap(err, "failed to add public key to account")
			}
		}
		return nil
	}

	publicKeys, err := sc.GetPublicKeys()
	if err != nil {
		return err
	}
	if len(publicKeys) == 0 {
		return fmt.Errorf("unable to list available public keys, is ssh-agent running?; do you need to run ssh-add?")
	}

	// Our signal handling under main() doesn't cause reading from stdin to cancel
	// as there's no way to pass app.ctx to stdin read calls.
	signal.Reset(syscall.SIGINT, syscall.SIGTERM)

	fmt.Printf("Which of the following keys do you want to register?\n")
	for i, key := range publicKeys {
		fmt.Printf("%d) %s\n", i+1, key.String())
	}
	keyNum := promptInput("enter key number (1=default): ")
	if keyNum == "" {
		keyNum = "1"
	}
	i, err := strconv.Atoi(keyNum)
	if err != nil {
		return errors.Wrap(err, "invalid key number")
	}
	if i <= 0 || i > len(publicKeys) {
		return fmt.Errorf("invalid key number")
	}
	publicKey := publicKeys[i-1].String()

	err = sc.AddPublickKey(publicKey)
	if err != nil {
		return errors.Wrap(err, "failed to add public key to account")
	}

	//switch over to new key if the user is currently using password-based auth
	email, authType, _, err := sc.WhoAmI()
	if err != nil {
		return errors.Wrap(err, "failed to validate auth token")
	}
	if authType == "password" {
		err = sc.SetLoginSSH(email, publicKey)
		if err != nil {
			app.console.Warnf("failed to authenticate using newly added public key: %s", err.Error())
			return nil
		}
		fmt.Printf("Switching from password-based login to ssh-based login\n")
	}

	return nil
}

func (app *earthlyApp) actionAccountRemoveKey(c *cli.Context) error {
	app.commandName = "accountRemoveKey"
	sc, err := secretsclient.NewClient(app.apiServer, app.sshAuthSock, app.authToken, app.console.Warnf)
	if err != nil {
		return errors.Wrap(err, "failed to create secretsclient")
	}
	for _, k := range c.Args().Slice() {
		err := sc.RemovePublickKey(k)
		if err != nil {
			return errors.Wrap(err, "failed to add public key to account")
		}
	}
	return nil
}
func (app *earthlyApp) actionAccountListTokens(c *cli.Context) error {
	app.commandName = "accountListTokens"
	sc, err := secretsclient.NewClient(app.apiServer, app.sshAuthSock, app.authToken, app.console.Warnf)
	if err != nil {
		return errors.Wrap(err, "failed to create secretsclient")
	}
	tokens, err := sc.ListTokens()
	if err != nil {
		return errors.Wrap(err, "failed to list account tokens")
	}
	if len(tokens) == 0 {
		return nil // avoid printing header columns when there are no tokens
	}

	now := time.Now()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "Token Name\tRead/Write\tExpiry\n")
	for _, token := range tokens {
		expired := now.After(token.Expiry)
		fmt.Fprintf(w, "%s", token.Name)
		if token.Write {
			fmt.Fprintf(w, "\trw")
		} else {
			fmt.Fprintf(w, "\tr")
		}
		fmt.Fprintf(w, "\t%s UTC", token.Expiry.UTC().Format("2006-01-02T15:04"))
		if expired {
			fmt.Fprintf(w, " *expired*")
		}
		fmt.Fprintf(w, "\n")
	}
	w.Flush()
	return nil
}
func (app *earthlyApp) actionAccountCreateToken(c *cli.Context) error {
	app.commandName = "accountCreateToken"
	if c.NArg() != 1 {
		return errors.New("invalid number of arguments provided")
	}

	var expiry time.Time
	if app.expiry == "" {
		expiry = time.Now().Add(time.Hour * 24 * 365)
	} else if app.expiry == "never" {
		expiry = time.Now().Add(time.Hour * 24 * 365 * 100) // TODO save this some other way
	} else {
		layouts := []string{
			"2006-01-02",
			time.RFC3339,
		}

		var err error
		for _, layout := range layouts {
			expiry, err = time.Parse(layout, app.expiry)
			if err == nil {
				break
			}
		}
		if err != nil {
			return fmt.Errorf("failed to parse expiry %q", app.expiry)
		}
	}

	sc, err := secretsclient.NewClient(app.apiServer, app.sshAuthSock, app.authToken, app.console.Warnf)
	if err != nil {
		return errors.Wrap(err, "failed to create secretsclient")
	}
	name := c.Args().First()
	token, err := sc.CreateToken(name, app.writePermission, &expiry)
	if err != nil {
		return errors.Wrap(err, "failed to create token")
	}
	expiryStr := humanize.Time(expiry)
	fmt.Printf("created token %q which will expire in %s; save this token somewhere, it can't be viewed again (only reset)\n", token, expiryStr)
	return nil
}
func (app *earthlyApp) actionAccountRemoveToken(c *cli.Context) error {
	app.commandName = "accountRemoveToken"
	if c.NArg() != 1 {
		return errors.New("invalid number of arguments provided")
	}
	name := c.Args().First()
	sc, err := secretsclient.NewClient(app.apiServer, app.sshAuthSock, app.authToken, app.console.Warnf)
	if err != nil {
		return errors.Wrap(err, "failed to create secretsclient")
	}
	err = sc.RemoveToken(name)
	if err != nil {
		return errors.Wrap(err, "failed to remove account tokens")
	}
	return nil
}

func (app *earthlyApp) actionAccountLogin(c *cli.Context) error {
	app.commandName = "accountLogin"
	email := app.email
	token := app.token
	pass := app.password

	if c.NArg() == 1 {
		emailOrToken := c.Args().First()
		if token == "" && email == "" {
			if secretsclient.IsValidEmail(emailOrToken) {
				email = emailOrToken
			} else {
				token = emailOrToken
			}

		} else {
			return errors.New("invalid number of arguments provided")
		}
	} else if c.NArg() > 1 {
		return errors.New("invalid number of arguments provided")
	}

	if token != "" && (email != "" || pass != "") {
		return errors.New("--token can not be used in conjuction with --email or --password")
	}
	sc, err := secretsclient.NewClient(app.apiServer, app.sshAuthSock, app.authToken, app.console.Warnf)
	if err != nil {
		return errors.Wrap(err, "failed to create secretsclient")
	}

	// special case where global auth token overrides login logic
	if app.authToken != "" {
		if email != "" || token != "" || pass != "" {
			return errors.New("account login flags have no effect when --auth-token (or the EARTHLY_TOKEN environment variable) is set")
		}
		loggedInEmail, authType, writeAccess, err := sc.WhoAmI()
		if err != nil {
			return errors.Wrap(err, "failed to validate auth token")
		}
		if !writeAccess {
			authType = "read-only-" + authType
		}
		fmt.Printf("Logged in as %q using %s auth\n", loggedInEmail, authType)
		return nil
	}

	if token != "" || pass != "" {
		err := sc.DeleteCachedCredentials()
		if err != nil {
			return errors.Wrap(err, "failed to clear cached credentials")
		}
		sc.DisableSSHKeyGuessing()
	} else if email != "" {
		foundSSHKeys, err := sc.FindSSHAuth()
		if err == nil {
			if keys, ok := foundSSHKeys[email]; ok {
				if len(keys) > 0 {
					foundSSHKey := keys[0]
					err := sc.SetLoginSSH(email, foundSSHKey)
					if err != nil {
						return err
					}
					fmt.Printf("Logged in as %q using ssh auth\n", email)
					return nil
				}
			}
		}
	}

	loggedInEmail, authType, writeAccess, err := sc.WhoAmI()
	switch errors.Cause(err) {
	case secretsclient.ErrNoAuthorizedPublicKeys, secretsclient.ErrNoSSHAgent:
		break
	case nil:
		if email != "" && email != loggedInEmail {
			break // case where a user has multiple emails and wants to switch
		}
		if !writeAccess {
			authType = "read-only-" + authType
		}
		fmt.Printf("Logged in as %q using %s auth\n", loggedInEmail, authType)
		return nil
	default:
		return err
	}

	if email == "" && token == "" {
		if app.sshAuthSock == "" {
			app.console.Warnf("No ssh auth socket detected; falling back to password-based login\n")
		}

		emailOrToken := promptInput("enter your email or auth token: ")
		if strings.Contains(emailOrToken, "@") {
			email = emailOrToken
		} else {
			token = emailOrToken
		}
	}

	if email != "" && pass == "" {
		passwordBytes, err := password.Read("enter your password: ")
		if err != nil {
			return err
		}
		pass = string(passwordBytes)
		if pass == "" {
			return fmt.Errorf("no password entered")
		}
	}

	if token != "" {
		email, err = sc.SetLoginToken(token)
		if err != nil {
			return err
		}
		fmt.Printf("Logged in as %q using token auth\n", email) // TODO display if using read-only token
	} else {
		err = sc.SetLoginCredentials(email, string(pass))
		if err != nil {
			return err
		}
		fmt.Printf("Logged in as %q using password auth\n", email)
		fmt.Printf("Warning unencrypted password has been stored under ~/.earthly/auth.token; consider using ssh-based auth to prevent this.\n")
	}
	return nil
}

func (app *earthlyApp) actionAccountLogout(c *cli.Context) error {
	app.commandName = "accountLogout"
	sc, err := secretsclient.NewClient(app.apiServer, app.sshAuthSock, app.authToken, app.console.Warnf)
	if err != nil {
		return err
	}
	err = sc.DeleteCachedCredentials()
	if err != nil {
		return errors.Wrap(err, "failed to logout")
	}
	return nil
}

func (app *earthlyApp) actionDebug(c *cli.Context) error {
	app.commandName = "debug"
	if c.NArg() > 1 {
		return errors.New("invalid number of arguments provided")
	}
	path := "."
	if c.NArg() == 1 {
		path = c.Args().First()
	}
	path = filepath.Join(path, "Earthfile")

	err := earthfile2llb.ParseDebug(path)
	if err != nil {
		return errors.Wrap(err, "parse debug")
	}
	return nil
}

func (app *earthlyApp) actionPrune(c *cli.Context) error {
	app.commandName = "prune"
	if c.NArg() != 0 {
		return errors.New("invalid arguments")
	}
	if app.pruneReset {
		// Prune by resetting container.
		if app.buildkitHost != "" {
			return errors.New("Cannot use prune --reset on non-default buildkit-host setting")
		}
		// Use twice the restart timeout for reset operations
		// (needs extra time to also remove the files).
		opTimeout := 2 * time.Duration(app.cfg.Global.BuildkitRestartTimeoutS) * time.Second
		err := buildkitd.ResetCache(
			c.Context, app.console, app.buildkitdImage, app.buildkitdSettings,
			opTimeout)
		if err != nil {
			return errors.Wrap(err, "reset cache")
		}
		return nil
	}

	// Prune via API.
	bkClient, _, err := app.newBuildkitdClient(c.Context)
	if err != nil {
		return errors.Wrap(err, "buildkitd new client")
	}
	defer bkClient.Close()
	var opts []client.PruneOption
	if app.pruneAll {
		opts = append(opts, client.PruneAll)
	}
	ch := make(chan client.UsageInfo, 1)
	eg, ctx := errgroup.WithContext(c.Context)
	eg.Go(func() error {
		err = bkClient.Prune(ctx, ch, opts...)
		if err != nil {
			return errors.Wrap(err, "buildkit prune")
		}
		close(ch)
		return nil
	})
	eg.Go(func() error {
		for {
			select {
			case _, ok := <-ch:
				if !ok {
					return nil
				}
				// TODO: Print some progress info.
			case <-ctx.Done():
				return nil
			}
		}
	})
	err = eg.Wait()
	if err != nil {
		return errors.Wrap(err, "err group")
	}
	return nil
}

func (app *earthlyApp) actionDocker2Earthly(c *cli.Context) error {
	return docker2earthly.Docker2Earthly(app.dockerfilePath, app.earthfilePath, app.earthfileFinalImage)
}

func (app *earthlyApp) actionBuild(c *cli.Context) error {
	app.commandName = "build"

	if app.ci {
		app.useInlineCache = true
		app.noOutput = true
		if app.remoteCache == "" && app.push {
			app.saveInlineCache = true
		}
	}
	if app.imageMode && app.artifactMode {
		return errors.New("both image and artifact modes cannot be active at the same time")
	}
	if (app.imageMode && app.noOutput) || (app.artifactMode && app.noOutput) {
		if app.ci {
			app.noOutput = false
		} else {
			return errors.New("cannot use --no-output with image or artifact modes")
		}
	}
	var target domain.Target
	var artifact domain.Artifact
	destPath := "./"
	if app.imageMode {
		if c.NArg() == 0 {
			cli.ShowAppHelp(c)
			return fmt.Errorf(
				"no image reference provided. Try %s --image +<target-name>", c.App.Name)
		} else if c.NArg() != 1 {
			cli.ShowAppHelp(c)
			return errors.New("invalid number of args")
		}
		targetName := c.Args().Get(0)
		var err error
		target, err = domain.ParseTarget(targetName)
		if err != nil {
			return errors.Wrapf(err, "parse target name %s", targetName)
		}
	} else if app.artifactMode {
		if c.NArg() == 0 {
			cli.ShowAppHelp(c)
			return fmt.Errorf(
				"no artifact reference provided. Try %s --artifact +<target-name>/<artifact-name>", c.App.Name)
		} else if c.NArg() != 1 && c.NArg() != 2 {
			cli.ShowAppHelp(c)
			return errors.New("invalid number of args")
		}
		artifactName := c.Args().Get(0)
		if c.NArg() == 2 {
			destPath = c.Args().Get(1)
		}
		var err error
		artifact, err = domain.ParseArtifact(artifactName)
		if err != nil {
			return errors.Wrapf(err, "parse artifact name %s", artifactName)
		}
		target = artifact.Target
	} else {
		if c.NArg() == 0 {
			cli.ShowAppHelp(c)
			return fmt.Errorf(
				"no target reference provided. Try %s +<target-name>", c.App.Name)
		} else if c.NArg() != 1 {
			cli.ShowAppHelp(c)
			return errors.New("invalid number of args")
		}
		targetName := c.Args().Get(0)
		var err error
		target, err = domain.ParseTarget(targetName)
		if err != nil {
			return errors.Wrapf(err, "parse target name %s", targetName)
		}
	}
	bkClient, bkIP, err := app.newBuildkitdClient(c.Context)
	if err != nil {
		return errors.Wrap(err, "buildkitd new client")
	}
	defer bkClient.Close()

	platformsSlice := make([]*specs.Platform, 0, len(app.platformsStr.Value()))
	for _, p := range app.platformsStr.Value() {
		platform, err := llbutil.ParsePlatform(p)
		if err != nil {
			return errors.Wrapf(err, "parse platform %s", p)
		}
		platformsSlice = append(platformsSlice, platform)
	}
	if len(platformsSlice) == 0 {
		platformsSlice = []*specs.Platform{nil}
	}

	dotEnvMap := make(map[string]string)
	if fileutil.FileExists(dotEnvPath) {
		dotEnvMap, err = godotenv.Read(dotEnvPath)
		if err != nil {
			return errors.Wrapf(err, "read %s", dotEnvPath)
		}
	}
	secretsMap, err := processSecrets(app.secrets.Value(), app.secretFiles.Value(), dotEnvMap)
	if err != nil {
		return err
	}

	debuggerSettings := debuggercommon.DebuggerSettings{
		DebugLevelLogging: app.debug,
		Enabled:           app.interactiveDebugging,
		RepeaterAddr:      fmt.Sprintf("%s:8373", bkIP),
		Term:              os.Getenv("TERM"),
	}

	debuggerSettingsData, err := json.Marshal(&debuggerSettings)
	if err != nil {
		return errors.Wrap(err, "debugger settings json marshal")
	}
	secretsMap[debuggercommon.DebuggerSettingsSecretsKey] = debuggerSettingsData

	sc, err := secretsclient.NewClient(app.apiServer, app.sshAuthSock, app.authToken, app.console.Warnf)
	if err != nil {
		return errors.Wrap(err, "failed to create secretsclient")
	}

	localhostProvider, err := localhostprovider.NewLocalhostProvider()
	if err != nil {
		return errors.Wrap(err, "failed to create localhostprovider")
	}

	cacheLocalDir, err := ioutil.TempDir("", "earthly-cache")
	if err != nil {
		return errors.Wrap(err, "make temp dir for cache")
	}
	defer os.RemoveAll(cacheLocalDir)
	defaultLocalDirs := make(map[string]string)
	defaultLocalDirs["earthly-cache"] = cacheLocalDir
	buildContextProvider := provider.NewBuildContextProvider()
	buildContextProvider.AddDirs(defaultLocalDirs)
	attachables := []session.Attachable{
		llbutil.NewSecretProvider(sc, secretsMap),
		authprovider.NewDockerAuthProvider(os.Stderr),
		buildContextProvider,
		localhostProvider,
	}

	gitLookup := buildcontext.NewGitLookup()
	err = app.updateGitLookupConfig(gitLookup)
	if err != nil {
		return err
	}

	if app.sshAuthSock != "" {
		ssh, err := sshprovider.NewSSHAgentProvider([]sshprovider.AgentConfig{{
			Paths: []string{app.sshAuthSock},
		}})
		if err != nil {
			return errors.Wrap(err, "ssh agent provider")
		}
		attachables = append(attachables, ssh)
	}

	var enttlmnts []entitlements.Entitlement
	if app.allowPrivileged {
		enttlmnts = append(enttlmnts, entitlements.EntitlementSecurityInsecure)
	}
	cleanCollection := cleanup.NewCollection()
	defer cleanCollection.Close()

	if app.interactiveDebugging {
		go terminal.ConnectTerm(c.Context, fmt.Sprintf("127.0.0.1:%d", app.buildkitdSettings.DebuggerPort))
	}

	varCollection, err := variables.ParseCommandLineBuildArgs(app.buildArgs.Value(), dotEnvMap)
	if err != nil {
		return errors.Wrap(err, "parse build args")
	}
	imageResolveMode := llb.ResolveModePreferLocal
	if app.pull {
		imageResolveMode = llb.ResolveModeForcePull
	}

	cacheImports := make(map[string]bool)
	if app.remoteCache != "" {
		cacheImports[app.remoteCache] = true
	}
	var cacheExport string
	var maxCacheExport string
	if app.remoteCache != "" && app.push {
		if app.maxRemoteCache {
			maxCacheExport = app.remoteCache
		} else {
			cacheExport = app.remoteCache
		}
	}
	builderOpts := builder.Opt{
		BkClient:             bkClient,
		Console:              app.console,
		Verbose:              app.verbose,
		Attachables:          attachables,
		Enttlmnts:            enttlmnts,
		NoCache:              app.noCache,
		CacheImports:         cacheImports,
		CacheExport:          cacheExport,
		MaxCacheExport:       maxCacheExport,
		UseInlineCache:       app.useInlineCache,
		SaveInlineCache:      app.saveInlineCache,
		SessionID:            app.sessionID,
		ImageResolveMode:     imageResolveMode,
		CleanCollection:      cleanCollection,
		VarCollection:        varCollection,
		BuildContextProvider: buildContextProvider,
		GitLookup:            gitLookup,
		UseFakeDep:           !app.noFakeDep,
	}
	b, err := builder.NewBuilder(c.Context, builderOpts)
	if err != nil {
		return errors.Wrap(err, "new builder")
	}

	if len(platformsSlice) != 1 {
		return errors.Errorf("multi-platform builds are not yet supported on the command line. You may, however, create a target with the instruction BUILD --plaform ... --platform ... %s", target)
	}
	buildOpts := builder.BuildOpt{
		PrintSuccess:          true,
		Push:                  app.push,
		NoOutput:              app.noOutput,
		OnlyFinalTargetImages: app.imageMode,
		Platform:              platformsSlice[0],
	}
	if app.artifactMode {
		buildOpts.OnlyArtifact = &artifact
		buildOpts.OnlyArtifactDestPath = destPath
	}
	_, err = b.BuildTarget(c.Context, target, buildOpts)
	if err != nil {
		return errors.Wrap(err, "build target")
	}
	return nil
}

func (app *earthlyApp) newBuildkitdClient(ctx context.Context, opts ...client.ClientOpt) (*client.Client, string, error) {
	if app.buildkitHost == "" {
		// Start our own.
		app.buildkitdSettings.Debug = app.debug
		opTimeout := time.Duration(app.cfg.Global.BuildkitRestartTimeoutS) * time.Second
		bkClient, err := buildkitd.NewClient(
			ctx, app.console, app.buildkitdImage, app.buildkitdSettings, opTimeout)
		if err != nil {
			return nil, "", errors.Wrap(err, "buildkitd new client (own)")
		}
		bkIP, err := buildkitd.GetContainerIP(ctx)
		if err != nil {
			return nil, "", errors.Wrap(err, "get container ip")
		}
		return bkClient, bkIP, nil
	}

	// Use provided.
	bkClient, err := client.New(ctx, app.buildkitHost, opts...)
	if err != nil {
		return nil, "", errors.Wrap(err, "buildkitd new client (provided)")
	}
	return bkClient, "", nil
}

func (app *earthlyApp) hasSSHKeys() bool {
	if app.sshAuthSock == "" {
		return false
	}

	agentSock, err := net.Dial("unix", app.sshAuthSock)
	if err != nil {
		return false
	}

	sshAgent := agent.NewClient(agentSock)
	keys, err := sshAgent.List()
	if err != nil {
		return false
	}

	return len(keys) > 0
}

func (app *earthlyApp) updateGitLookupConfig(gitLookup *buildcontext.GitLookup) error {

	autoProtocol := "ssh"
	if !app.hasSSHKeys() {
		app.console.Printf("No ssh auth socket detected or zero keys loaded; falling back to https for auto auth values\n")
		autoProtocol = "https"

		// convert all ssh to https for pre-configured instances
		gitLookup.DisableSSH()
	}

	for k, v := range app.cfg.Git {
		if k == "github" || k == "gitlab" || k == "bitbucket" {
			app.console.Warnf("git configuration for %q found, did you mean %q?\n", k, k+".com")
		}
		pattern := v.Pattern
		if pattern == "" {
			// if empty, assume it will be of the form host.com/user/repo.git
			host := k
			if !strings.Contains(host, ".") {
				host += ".com"
			}
			pattern = host + "/[^/]+/[^/]+"
		}
		auth := v.Auth
		if auth == "auto" {
			auth = autoProtocol
		}
		suffix := v.Suffix
		if suffix == "" {
			suffix = ".git"
		}
		err := gitLookup.AddMatcher(k, pattern, v.Substitute, v.User, v.Password, suffix, auth, v.KeyScan)
		if err != nil {
			return errors.Wrap(err, "gitlookup")
		}
	}
	return nil
}

func processSecrets(secrets, secretFiles []string, dotEnvMap map[string]string) (map[string][]byte, error) {
	finalSecrets := make(map[string][]byte)
	for k, v := range dotEnvMap {
		finalSecrets[k] = []byte(v)
	}
	for _, secret := range secrets {
		parts := strings.SplitN(secret, "=", 2)
		key := parts[0]
		var data []byte
		if len(parts) == 2 {
			// secret value passed as argument
			data = []byte(parts[1])
		} else {
			// Not set. Use environment to fetch it.
			value, found := os.LookupEnv(secret)
			if !found {
				return nil, fmt.Errorf("env var %s not set", secret)
			}
			data = []byte(value)
		}
		if _, ok := finalSecrets[key]; ok {
			return nil, fmt.Errorf("secret %q already contains a value", key)
		}
		finalSecrets[key] = data
	}
	for _, secret := range secretFiles {
		parts := strings.SplitN(secret, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("unable to parse --secret-file argument: %q", secret)
		}
		k := parts[0]
		path := parts[1]
		data, err := ioutil.ReadFile(path)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to open %q", path)
		}
		if _, ok := finalSecrets[k]; ok {
			return nil, fmt.Errorf("secret %q already contains a value", k)
		}
		finalSecrets[k] = []byte(data)
	}
	return finalSecrets, nil
}

func defaultConfigPath() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		panic(err)
	}

	oldConfig := filepath.Join(homeDir, ".earthly", "config.yaml")
	newConfig := filepath.Join(homeDir, ".earthly", "config.yml")
	if fileutil.FileExists(oldConfig) && !fileutil.FileExists(newConfig) {
		return oldConfig
	}
	return newConfig
}
