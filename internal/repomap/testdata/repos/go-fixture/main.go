package main

import "fmt"

func main() {
	fmt.Println(Greet("world"))
}

// Greet builds a greeting.
func Greet(name string) string {
	return "hi " + name
}
