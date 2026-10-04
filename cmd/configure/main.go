package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/jcarcaboso/cliproxy-quota-balancer/internal/deployconfig"
)

func main() {
	config := flag.String("config", "/config/config.yaml", "Persisted host configuration")
	policy := flag.String("policy", "/etc/quota-balancer.yaml", "Plugin policy")
	flag.Parse()
	if errApply := deployconfig.Apply(*config, *policy); errApply != nil {
		// Do not include YAML bodies or credentials in logs.
		fmt.Fprintln(os.Stderr, "quota-balancer configuration update failed:", errApply)
		os.Exit(1)
	}
}
