package main

import (
	"net/http"
	"time"

	"github.com/jrockway/alertmanager-status/status"
	"github.com/jrockway/opinionated-server/server"
	"go.uber.org/zap"
)

type config struct {
	Threshold time.Duration `long:"threshold" short:"t" env:"ALERTMANAGER_STATUS_THRESHOLD" description:"How long to wait before considering Alertmanager unhealthy." default:"60s"`
}

func main() {
	var cfg config
	server.AppName = "alertmanager-status"
	server.AddFlagGroup("General", &cfg)

	mux := http.NewServeMux()
	server.SetHTTPHandler(mux)
	server.Setup()

	w := status.NewWatcher(zap.L().Named("watcher"), "alertmanager", cfg.Threshold)
	http.HandleFunc("/livez", w.HandleLiveness)           // internal
	http.HandleFunc("/webhook", w.HandleAlertmanagerPing) // internal
	mux.HandleFunc("/", w.HandleHealthCheck)              // public
	server.AddDrainHandler(func() { w.Stop() })

	server.ListenAndServe()
}
