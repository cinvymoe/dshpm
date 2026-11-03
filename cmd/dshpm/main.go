// Command dshpm manages DeepSeek Harness plugins installed in a DSH profile.
package main

import (
	"os"

	"github.com/cinvymoe/dshpm/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
