// Command demo runs the example service. The whole lifecycle lives in
// verticle.Run — main is just a thin entrypoint.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/smallnest/matex/internal/app"
	"github.com/smallnest/matex/pkg/core/verticle"
)

func main() {
	conf := flag.String("conf", "", "config file (default configs/config.yaml or $CONFIG_FILE)")
	flag.Parse()

	if err := verticle.Run(context.Background(), &app.DemoService{}, verticle.WithConf(*conf)); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}
