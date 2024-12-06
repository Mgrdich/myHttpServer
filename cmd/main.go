package main

import (
	"flag"
	"log"
	"net/http"

	"myHttpServer/pkg"
)

func main() {
	// Define flags
	addr := flag.String("addr", ":8080", "Address to listen on")
	useTLS := flag.Bool("tls", false, "Enable TLS (true/false)")

	flag.Parse()

	// Define the handler
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte("Hello World"))
		if err != nil {
			log.Printf("Error writing response: %v", err)
		}
	})

	// Conditional logic based on the useTLS flag
	if *useTLS {
		log.Printf("Starting server with TLS on %s", *addr)
		err := pkg.ListenAndServeTLS(*addr, "server.crt", "server.key", true, handler)
		if err != nil {
			log.Panic(err)
		}
	} else {
		log.Printf("Starting server without TLS on %s", *addr)
		err := pkg.ListenAndServe(*addr, handler)
		if err != nil {
			log.Panic(err)
		}
	}
}
