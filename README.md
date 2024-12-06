# HTTP Server Implementation from Scratch
This code is inspired from the Golang standard library, 
implements restricted of `Server` that implements `HTTP/1.1` and `HTTP/2.0` protocols 


* implement `HTTP/1.1` Server using `tcp` and you can run it under `TLS` as well 
* implement `HTTP/2.0` Server using `tcp` ( still work in progress )
* making sure it uses multi-threading and connection persistence for each case as defined per protocol

To use `HTTP/2.0` you should use the `TLS` Version (which is why `ListenAndServeTLS` is used instead of `ListenAndServe`)


## How to build the project
* Create self signed ssl certificates by running `openssl req -new -x509 -days 365 -nodes -out server.crt -keyout server.key`
* run `make build` to build the executable and then `./main-exec`
* To use the tls `./main-exec -tls=true` by default it is false
* to configure the port that you want to run `./main-exec -addr=8081`