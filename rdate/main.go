package main

import (
	"fmt"
	"time"
)

func main() {
	fmt.Printf("%s\n", time.Now().Format(time.RFC3339))
}
