package main

import (
	"github.com/sagernet/sing-box/smartroute"
	"github.com/spf13/cobra"
)

func init() {
	origPreRun := mainCommand.PersistentPreRun
	mainCommand.PersistentPreRun = func(cmd *cobra.Command, args []string) {
		if origPreRun != nil {
			origPreRun(cmd, args)
		}
		globalCtx = smartroute.Context(globalCtx)
	}
}
