// Command autopilot runs the SigNoz Incident Autopilot controller.
package main

import (
	"flag"
	"log"

	"github.com/harsh3dev/signoz/incident-autopilot/internal/config"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the autopilot configuration file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	log.Printf("incident-autopilot starting: service=%s namespace=%s mode=%s",
		cfg.Target.Service, cfg.Target.Namespace, cfg.Controller.Mode)
}
