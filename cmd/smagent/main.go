package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("smagent", version)
		return
	}
	fmt.Fprintln(os.Stderr, "smagent: not wired yet")
	os.Exit(1)
}
