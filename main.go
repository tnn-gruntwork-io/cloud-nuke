package main

import (
	"github.com/tnn-gruntwork-io/cloud-nuke/commands"
	"github.com/tnn-gruntwork-io/go-commons/entrypoint"
)

// VERSION - Set at build time
var VERSION string
var MixPanelClientId string

func main() {
	app := commands.CreateCli(VERSION, MixPanelClientId)
	entrypoint.RunApp(app)
}
