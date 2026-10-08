// Command yoho deploys Docker Compose apps to servers over SSH.
package main

import (
	"os"

	"github.com/yoho-dev/yoho/internal/cli"
)

func main() { os.Exit(cli.Execute()) }
