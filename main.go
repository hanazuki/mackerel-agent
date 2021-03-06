package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Songmu/prompter"
	"github.com/mackerelio/mackerel-agent/command"
	"github.com/mackerelio/mackerel-agent/config"
	"github.com/mackerelio/mackerel-agent/logging"
	"github.com/mackerelio/mackerel-agent/mackerel"
	"github.com/mackerelio/mackerel-agent/version"
)

// allow options like -role=... -role=...
type roleFullnamesFlag []string

var roleFullnamePattern = regexp.MustCompile(`^[a-zA-Z0-9][-_a-zA-Z0-9]*:\s*[a-zA-Z0-9][-_a-zA-Z0-9]*$`)

func (r *roleFullnamesFlag) String() string {
	return fmt.Sprint(*r)
}

func (r *roleFullnamesFlag) Set(input string) error {
	inputRoles := strings.Split(input, ",")
	*r = append(*r, inputRoles...)
	return nil
}

type otherOptions struct {
	printVersion bool
	runOnce      bool
}

var logger = logging.GetLogger("main")

const (
	exitStatusOK = iota
	exitStatusError
)

func main() {
	os.Exit(dispatch(os.Args[1:]))
}

// empty string is dealt with the key of main process
const mainProcess = ""

// subcommands and processes of the mackerel-agent
var commands = map[string](func([]string) int){
	mainProcess: doMain,
	"version":   doVersion,
	"retire":    doRetire,
}

func doVersion(_ []string) int {
	fmt.Printf("mackerel-agent version %s (rev %s) [%s %s %s] \n",
		version.VERSION, version.GITCOMMIT, runtime.GOOS, runtime.GOARCH, runtime.Version())
	return exitStatusOK
}

func doMain(argv []string) int {
	conf, otherOpts := resolveConfig(argv)
	if conf == nil {
		return exitStatusError
	}
	if otherOpts != nil && otherOpts.printVersion {
		return doVersion([]string{})
	}

	if conf.Verbose {
		logging.SetLogLevel(logging.DEBUG)
	}

	logger.Infof("Starting mackerel-agent version:%s, rev:%s, apibase:%s", version.VERSION, version.GITCOMMIT, conf.Apibase)

	if otherOpts != nil && otherOpts.runOnce {
		command.RunOnce(conf)
		return exitStatusOK
	}

	if conf.Apikey == "" {
		logger.Criticalf("Apikey must be specified in the command-line flag or in the config file")
		return exitStatusError
	}
	return start(conf)
}

func doRetire(argv []string) int {
	conf, force, err := resolveConfigForRetire(argv)
	if err != nil {
		return exitStatusError
	}
	if conf.Apikey == "" {
		logger.Criticalf("Apikey must be specified in the command-line flag or in the config file")
		return exitStatusError
	}

	hostID, err := command.LoadHostID(conf.Root)
	if err != nil {
		logger.Warningf("HostID file is not found")
		return exitStatusError
	}

	api, err := mackerel.NewAPI(conf.Apibase, conf.Apikey, conf.Verbose)
	if err != nil {
		logger.Errorf("failed to create api client: %s", err)
		return exitStatusError
	}

	if !force && !prompter.YN(fmt.Sprintf("retire this host? (hostID: %s)", hostID), false) {
		logger.Infof("Retirement is canceled.")
		return exitStatusError
	}

	err = api.RetireHost(hostID)
	if err != nil {
		logger.Errorf("failed to retire the host: %s", err)
		return exitStatusError
	}
	logger.Infof("This host (hostID: %s) has been retired.", hostID)
	// just to try to remove hostID file.
	err = command.RemoveIDFile(conf.Root)
	if err != nil {
		logger.Warningf("Failed to remove HostID file: %s", err)
	}
	return exitStatusOK
}

func resolveConfigForRetire(argv []string) (*config.Config, bool, error) {
	fs := flag.NewFlagSet("mackerel-agent retire", flag.ExitOnError)
	// Allow accepting unnecessary options, pidfile, diagnostic and role.
	// Because, these options are potentially passed in initd script by using $OTHER_OPTS. dirty...
	var (
		conffile = fs.String("conf", config.DefaultConfig.Conffile, "Config file path (Configs in this file are over-written by command line options)")
		apibase  = fs.String("apibase", config.DefaultConfig.Apibase, "API base")
		_        = fs.String("pidfile", config.DefaultConfig.Pidfile, "(not used in retire)")
		root     = fs.String("root", config.DefaultConfig.Root, "Directory containing variable state information")
		apikey   = fs.String("apikey", "", "API key from mackerel.io web site")
		force    = fs.Bool("force", false, "force retirement without prompting")
		_        = fs.Bool("diagnostic", false, "(not used in retire)")
	)
	var roleFullnames roleFullnamesFlag
	fs.Var(&roleFullnames, "role", "(not used in retire)")
	var verbose bool
	fs.BoolVar(&verbose, "verbose", config.DefaultConfig.Verbose, "Toggle verbosity")
	fs.BoolVar(&verbose, "v", config.DefaultConfig.Verbose, "Toggle verbosity (shorthand)")
	fs.Parse(argv)
	conf, err := config.LoadConfig(*conffile)
	if err != nil {
		return nil, *force, err
	}
	// overwrite config from file by config from args
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "apibase":
			conf.Apibase = *apibase
		case "apikey":
			conf.Apikey = *apikey
		case "root":
			conf.Root = *root
		case "verbose", "v":
			conf.Verbose = verbose
		}
	})
	return conf, *force, nil
}

// resolveConfig parses command line arguments and loads config file to
// return config.Config information.
// As a special case, if `-version` flag is given it stops processing
// and return true for the second return value.
func resolveConfig(argv []string) (*config.Config, *otherOptions) {
	conf := &config.Config{}
	otherOptions := &otherOptions{}

	fs := flag.NewFlagSet("mackerel-agent", flag.ExitOnError)

	var (
		conffile     = fs.String("conf", config.DefaultConfig.Conffile, "Config file path (Configs in this file are over-written by command line options)")
		apibase      = fs.String("apibase", config.DefaultConfig.Apibase, "API base")
		pidfile      = fs.String("pidfile", config.DefaultConfig.Pidfile, "File containing PID")
		root         = fs.String("root", config.DefaultConfig.Root, "Directory containing variable state information")
		apikey       = fs.String("apikey", "", "API key from mackerel.io web site")
		diagnostic   = fs.Bool("diagnostic", false, "Enables diagnostic features")
		runOnce      = fs.Bool("once", false, "Show spec and metrics to stdout once")
		printVersion = fs.Bool("version", false, "Prints version and exit")
	)

	var verbose bool
	fs.BoolVar(&verbose, "verbose", config.DefaultConfig.Verbose, "Toggle verbosity")
	fs.BoolVar(&verbose, "v", config.DefaultConfig.Verbose, "Toggle verbosity (shorthand)")

	// The value of "role" option is internally "roll fullname",
	// but we call it "role" here for ease.
	var roleFullnames roleFullnamesFlag
	fs.Var(&roleFullnames, "role", "Set this host's roles (format: <service>:<role>)")
	fs.Parse(argv)

	if *printVersion {
		otherOptions.printVersion = true
		return conf, otherOptions
	}

	if *runOnce {
		otherOptions.runOnce = true
		return conf, otherOptions
	}

	conf, confErr := config.LoadConfig(*conffile)
	if confErr != nil {
		logger.Criticalf("Failed to load the config file: %s", confErr)
		return nil, nil
	}

	// overwrite config from file by config from args
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "apibase":
			conf.Apibase = *apibase
		case "apikey":
			conf.Apikey = *apikey
		case "pidfile":
			conf.Pidfile = *pidfile
		case "root":
			conf.Root = *root
		case "diagnostic":
			conf.Diagnostic = *diagnostic
		case "verbose", "v":
			conf.Verbose = verbose
		case "role":
			conf.Roles = roleFullnames
		}
	})

	r := []string{}
	for _, roleFullName := range conf.Roles {
		if !roleFullnamePattern.MatchString(roleFullName) {
			logger.Errorf("Bad format for role fullname (expecting <service>:<role>. Alphabet, numbers, hyphens and underscores are acceptable, but the first character must not be a hyphen or an underscore.): '%s'", roleFullName)
		} else {
			r = append(r, roleFullName)
		}
	}
	conf.Roles = r
	return conf, nil
}

func createPidFile(pidfile string) error {
	if pidString, err := ioutil.ReadFile(pidfile); err == nil {
		if pid, err := strconv.Atoi(string(pidString)); err == nil {
			if existsPid(pid) {
				return fmt.Errorf("Pidfile found, try stopping another running mackerel-agent or delete %s", pidfile)
			}
			// Note mackerel-agent in windows can't remove pidfile during stoping the service
			logger.Warningf("Pidfile found, but there seems no another process of mackerel-agent. Ignoring %s", pidfile)
		} else {
			logger.Warningf("Malformed pidfile found. Ignoring %s", pidfile)
		}
	}

	file, err := os.Create(pidfile)
	if err != nil {
		logger.Criticalf("Failed to create a pidfile: %s", err)
		return err
	}
	defer file.Close()

	_, err = fmt.Fprintf(file, "%d", os.Getpid())
	return err
}

func removePidFile(pidfile string) {
	if err := os.Remove(pidfile); err != nil {
		logger.Errorf("Failed to remove the pidfile: %s: %s", pidfile, err)
	}
}

func start(conf *config.Config) int {
	if err := createPidFile(conf.Pidfile); err != nil {
		return exitStatusError
	}
	defer removePidFile(conf.Pidfile)

	ctx, err := command.Prepare(conf)
	if err != nil {
		logger.Criticalf(err.Error())
		return exitStatusError
	}

	termCh := make(chan struct{})
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP)
	go signalHandler(c, ctx, termCh)

	return command.Run(ctx, termCh)
}

var maxTerminatingInterval = 30 * time.Second

func signalHandler(c chan os.Signal, ctx *command.Context, termCh chan struct{}) {
	received := false
	for sig := range c {
		if sig == syscall.SIGHUP {
			logger.Debugf("Received signal '%v'", sig)
			// TODO reload configuration file

			ctx.UpdateHostSpecs()
		} else {
			if !received {
				received = true
				logger.Infof(
					"Received signal '%v', try graceful shutdown up to %f seconds. If you want force shutdown immediately, send a signal again.",
					sig,
					maxTerminatingInterval.Seconds())
			} else {
				logger.Infof("Received signal '%v' again, force shutdown.", sig)
			}
			termCh <- struct{}{}
			go func() {
				time.Sleep(maxTerminatingInterval)
				logger.Infof("Timed out. force shutdown.")
				termCh <- struct{}{}
			}()
		}
	}
}
