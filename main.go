package main

import (
	"flag"
	"log/slog"
	"os"

	"github.com/docker/go-plugins-helpers/network"

	"github.com/acul21/docker-gwctr/gwctr"
)

const pluginName = "gwctr"

func main() {
	debug := flag.Bool("debug", false, "enable debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	d, err := gwctr.NewDriver()
	if err != nil {
		slog.Error("driver init failed", "err", err)
		os.Exit(1)
	}

	h := network.NewHandler(d)
	slog.Info("serving plugin", "socket", "/run/docker/plugins/"+pluginName+".sock")
	if err := h.ServeUnix(pluginName, 0); err != nil {
		slog.Error("serve failed", "err", err)
		os.Exit(1)
	}
}
