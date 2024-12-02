package main

import (
	"log"
	"net/http"

	"myHttpServer/pkg"
)

func main() {
	// own package
	//err := pkg.ListenAndServerTLS(
	//	"https://127.0.0.1:8080",
	//	"server.crt",
	//	"server.key",
	//	true,
	//	http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	//		log.Println(w, r)
	//	}))
	//
	//if err != nil {
	//	log.Panic(err)
	//}
	err := pkg.ListenAndServer("127.0.0.1:8080",
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.Println(w, r)
		}))

	if err != nil {
		log.Panic(err)
	}
}
