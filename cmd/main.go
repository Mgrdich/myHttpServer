package main

import (
	"net/http"
	"time"
)

func main() {
	http.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {})

	server := &http.Server{
		Addr:              "127.0.0.1:8080",
		ReadHeaderTimeout: 3 * time.Second,
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {

		}),
	}

	err := server.ListenAndServe()
	if err != nil {
		return
	}
}
