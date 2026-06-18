package main

import (
	"os"

	"github.com/peios/pekit/internal/pekit"
)

func main() {
	os.Exit(pekit.Main(os.Args[1:], os.Stdout, os.Stderr))
}
