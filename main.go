// Command kloo is an autonomous coding CLI for small local LLMs.
//
// main is intentionally thin: it builds the cobra root command (wired in
// internal/cli) and executes it. All behaviour lives under internal/**.
package main

import "github.com/lokal/kloo/internal/cli"

func main() {
	cli.Execute()
}
