package main

import (
	"indonesia-stock/internal/interfaces/cli"
	"os"
)

func main() {
	code := cli.Run(os.Args)
	os.Exit(code)
}
