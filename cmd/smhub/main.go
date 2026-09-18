package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("smhub", version)
		return
	}
	fmt.Fprintln(os.Stderr, "smhub: not wired yet")
	os.Exit(1)
}
