package main

import (
	"fmt"
	"uuid"
)

func main() {
	fmt.Printf("%s\n", uuid.New().String())
}
