package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	runtimeDebug "runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	box "github.com/sagernet/sing-box"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/experimental/deprecated"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/smartroute"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/json/badjson"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/filemanager"
	"github.com/spf13/cobra"
)

var (
	globalCtx         context.Context
	configPaths       []string
	configDirectories []string
	workingDir        string
	disableColor      bool
)

var mainCommand = &cobra.Command{
	Use:              "smartroute",
	Short:            "sing-box with SmartRoute extension",
	PersistentPreRun: preRun,
}

func init() {
	mainCommand.PersistentFlags().StringArrayVarP(&configPaths, "config", "c", nil, "set configuration file path")
	mainCommand.PersistentFlags().StringArrayVarP(&configDirectories, "config-directory", "C", nil, "set configuration directory path")
	mainCommand.PersistentFlags().StringVarP(&workingDir, "directory", "D", "", "set working directory")
	mainCommand.PersistentFlags().BoolVarP(&disableColor, "disable-color", "", false, "disable color output")

	mainCommand.AddCommand(&cobra.Command{
		Use:   "run",
		Short: "Run service",
		Run: func(cmd *cobra.Command, args []string) {
			if err := runService(); err != nil {
				log.Fatal(err)
			}
		},
	})
	mainCommand.AddCommand(&cobra.Command{
		Use:   "check",
		Short: "Check configuration",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			if err := checkConfig(); err != nil {
				log.Fatal(err)
			}
		},
	})
	mainCommand.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print current version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("sing-box version %s (with smartroute extension)\n", C.Version)
		},
	})
}

func main() {
	if err := mainCommand.Execute(); err != nil {
		log.Fatal(err)
	}
}

func preRun(cmd *cobra.Command, args []string) {
	globalCtx = context.Background()
	sudoUser := os.Getenv("SUDO_USER")
	sudoUID, _ := strconv.Atoi(os.Getenv("SUDO_UID"))
	sudoGID, _ := strconv.Atoi(os.Getenv("SUDO_GID"))
	if sudoUID == 0 && sudoGID == 0 && sudoUser != "" {
		sudoUserObject, _ := user.Lookup(sudoUser)
		if sudoUserObject != nil {
			sudoUID, _ = strconv.Atoi(sudoUserObject.Uid)
			sudoGID, _ = strconv.Atoi(sudoUserObject.Gid)
		}
	}
	if sudoUID > 0 && sudoGID > 0 {
		globalCtx = filemanager.WithDefault(globalCtx, "", "", sudoUID, sudoGID)
	}
	if disableColor {
		logFactory := log.NewDefaultFactory(context.Background(), log.Formatter{BaseTime: time.Now(), DisableColors: true}, os.Stderr, "", nil, false)
		common.Must(logFactory.Start())
		log.SetStdLogger(logFactory.Logger())
	}
	if workingDir != "" {
		if _, err := os.Stat(workingDir); err != nil {
			_ = filemanager.MkdirAll(globalCtx, workingDir, 0o777)
		}
		if err := os.Chdir(workingDir); err != nil {
			log.Fatal(err)
		}
	}
	if len(configPaths) == 0 && len(configDirectories) == 0 {
		configPaths = append(configPaths, "config.json")
	}
	globalCtx = smartroute.Context(service.ContextWith(globalCtx, deprecated.NewStderrManager(log.StdLogger())))
}

type optionsEntry struct {
	content []byte
	path    string
	options option.Options
}

func readConfigAt(path string) (*optionsEntry, error) {
	var (
		configContent []byte
		err           error
	)
	if path == "stdin" {
		configContent, err = io.ReadAll(os.Stdin)
	} else {
		configContent, err = os.ReadFile(path)
	}
	if err != nil {
		return nil, E.Cause(err, "read config at ", path)
	}
	options, err := json.UnmarshalExtendedContext[option.Options](globalCtx, configContent)
	if err != nil {
		return nil, E.Cause(err, "decode config at ", path)
	}
	return &optionsEntry{
		content: configContent,
		path:    path,
		options: options,
	}, nil
}

func readConfigAndMerge() (option.Options, error) {
	var optionsList []*optionsEntry
	for _, path := range configPaths {
		entry, err := readConfigAt(path)
		if err != nil {
			return option.Options{}, err
		}
		optionsList = append(optionsList, entry)
	}
	for _, directory := range configDirectories {
		entries, err := os.ReadDir(directory)
		if err != nil {
			return option.Options{}, E.Cause(err, "read config directory at ", directory)
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".json") || entry.IsDir() {
				continue
			}
			optEntry, err := readConfigAt(filepath.Join(directory, entry.Name()))
			if err != nil {
				return option.Options{}, err
			}
			optionsList = append(optionsList, optEntry)
		}
	}
	sort.Slice(optionsList, func(i, j int) bool {
		return optionsList[i].path < optionsList[j].path
	})
	if len(optionsList) == 1 {
		return optionsList[0].options, nil
	}
	var (
		mergedMessage json.RawMessage
		err           error
	)
	for _, opt := range optionsList {
		mergedMessage, err = badjson.MergeJSON(globalCtx, opt.options.RawMessage, mergedMessage, false)
		if err != nil {
			return option.Options{}, E.Cause(err, "merge config at ", opt.path)
		}
	}
	var mergedOptions option.Options
	err = mergedOptions.UnmarshalJSONContext(globalCtx, mergedMessage)
	if err != nil {
		return option.Options{}, E.Cause(err, "unmarshal merged config")
	}
	return mergedOptions, nil
}

func checkConfig() error {
	options, err := readConfigAndMerge()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(service.ExtendContext(globalCtx))
	defer cancel()
	instance, err := smartroute.NewBox(box.Options{
		Context: ctx,
		Options: options,
	})
	if err == nil {
		_ = instance.Close()
	}
	return err
}

func createService(options option.Options) (*box.Box, context.CancelFunc, error) {
	if disableColor {
		if options.Log == nil {
			options.Log = &option.LogOptions{}
		}
		options.Log.DisableColor = true
	}
	ctx, cancel := context.WithCancel(service.ExtendContext(globalCtx))
	instance, err := smartroute.NewBox(box.Options{
		Context: ctx,
		Options: options,
	})
	if err != nil {
		cancel()
		return nil, nil, E.Cause(err, "create service")
	}
	if err := instance.Start(); err != nil {
		cancel()
		return nil, nil, E.Cause(err, "start service")
	}
	return instance, cancel, nil
}

func runService() error {
	options, err := readConfigAndMerge()
	if err != nil {
		return err
	}
	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(osSignals)
	for {
		instance, cancel, createErr := createService(options)
		if createErr != nil {
			return createErr
		}
		runtimeDebug.FreeOSMemory()
		for {
			osSignal := <-osSignals
			if osSignal == syscall.SIGHUP {
				if err := checkConfig(); err != nil {
					log.Error(E.Cause(err, "reload service"))
					continue
				}
			}
			cancel()
			err = instance.Close()
			if osSignal != syscall.SIGHUP {
				if err != nil {
					log.Error(E.Cause(err, "sing-box did not close properly"))
				}
				return nil
			}
			break
		}
		options, err = readConfigAndMerge()
		if err != nil {
			return err
		}
	}
}
