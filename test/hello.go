//go:build !A && !B

package main

import "fmt"

func hello() {
	fmt.Println("Hello unknown!")
}
