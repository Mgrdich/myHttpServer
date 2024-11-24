# HTTP Server Implementation from Scratch
This code is inspired from the Golang standard library, 
implements restricted of `Server` that implements `HTTP/1.1` and `HTTP/2.0` protocols 


* implement `HTTP/1.1` Server using `tcp`
* implement `HTTP/2.0` Server using `tcp`
* making sure it uses multi-threading and connection persistence for each case as defined per protocol

To use `HTTP/2.0` you should use the `TLS` Version (which is why `ListenAndServeTLS` is used instead of `ListenAndServe`)


## How to build the project
* Create self signed ssl certificates by running `openssl req -new -x509 -days 365 -nodes -out server.crt -keyout server.key`
* run `make build_ci` to build the executable and then `./main-exec`
* or you can `make run`