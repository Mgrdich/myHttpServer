package main

import (
	"fmt"
	"net"
)

func main() {
	_, err := net.Listen("tcp", "127.0.0.1:8080")
	if err != nil {
		// handle error
		fmt.Println(err)
	}
}
